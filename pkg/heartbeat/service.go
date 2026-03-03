// Package heartbeat provides a periodic background agent runner.
// Matching TS src/infra/heartbeat-runner.ts.
//
// The heartbeat wakes the agent at regular intervals so it can check on
// things (calendar, inbox, alerts) and surface anything that needs attention.
// If nothing needs attention the agent replies with HEARTBEAT_OK which is
// silently dropped.
package heartbeat

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/pkg/bus"
	"github.com/nextlevelbuilder/goclaw/pkg/config"
)

// Default heartbeat prompt matching TS.
const defaultPrompt = "Read HEARTBEAT.md if it exists (workspace context). Follow it strictly. " +
	"Do not infer or repeat old tasks from prior chats. " +
	"If nothing needs attention, reply HEARTBEAT_OK."

const defaultInterval = 30 * time.Minute

// DefaultInterval returns the default heartbeat interval (30m).
func DefaultInterval() time.Duration { return defaultInterval }
const defaultAckMaxChars = 300
const heartbeatOKToken = "HEARTBEAT_OK"

// AgentRunner is the callback the service uses to run an agent turn.
// It returns the agent's response text and an error.
type AgentRunner func(ctx context.Context, agentID, sessionKey, message, runID string) (string, error)

// DeliveryTarget holds resolved delivery info for a heartbeat alert.
type DeliveryTarget struct {
	Channel string
	ChatID  string
}

// LastUsedResolver returns the last-used channel + chatID for an agent.
// Returns ("", "") if unknown.
type LastUsedResolver func(agentID string) (channel, chatID string)

// Config holds resolved runtime config for the heartbeat service.
type Config struct {
	AgentID      string
	Interval     time.Duration
	ActiveHours  *config.ActiveHoursConfig
	Model        string // unused for now, reserved for model override
	SessionKey   string
	Target       string // "last", "none", or channel name
	To           string // explicit chat ID
	Prompt       string
	AckMaxChars  int
	Workspace    string // for HEARTBEAT.md detection
}

// Service manages the periodic heartbeat loop.
type Service struct {
	cfg         Config
	runner      AgentRunner
	msgBus      *bus.MessageBus
	lastUsed    LastUsedResolver
	mu          sync.Mutex
	running     bool
	cancel      context.CancelFunc
	lastContent string    // dedup: last non-OK content
	lastAlertAt time.Time // dedup: when last alert was sent
}

// NewService creates a heartbeat service.
func NewService(cfg Config, runner AgentRunner, msgBus *bus.MessageBus, lastUsed LastUsedResolver) *Service {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.Prompt == "" {
		cfg.Prompt = defaultPrompt
	}
	if cfg.AckMaxChars <= 0 {
		cfg.AckMaxChars = defaultAckMaxChars
	}
	if cfg.SessionKey == "" {
		cfg.SessionKey = fmt.Sprintf("agent:%s:heartbeat:main", cfg.AgentID)
	}
	if cfg.Target == "" {
		cfg.Target = "last"
	}

	return &Service{
		cfg:      cfg,
		runner:   runner,
		msgBus:   msgBus,
		lastUsed: lastUsed,
	}
}

// Start begins the heartbeat loop in a background goroutine.
func (s *Service) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.running = true

	go s.loop(ctx)
	slog.Info("heartbeat service started",
		"agent", s.cfg.AgentID,
		"interval", s.cfg.Interval,
		"target", s.cfg.Target,
	)
}

// Stop halts the heartbeat loop.
func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return
	}

	s.cancel()
	s.running = false
	slog.Info("heartbeat service stopped", "agent", s.cfg.AgentID)
}

// IsRunning returns whether the heartbeat loop is active.
func (s *Service) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// --- Internal loop ---

func (s *Service) loop(ctx context.Context) {
	// Initial delay: wait one full interval before first heartbeat
	// (matching TS behavior — don't fire immediately on startup).
	timer := time.NewTimer(s.cfg.Interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.tick(ctx)
			timer.Reset(s.cfg.Interval)
		}
	}
}

func (s *Service) tick(ctx context.Context) {
	// Check active hours
	if s.cfg.ActiveHours != nil && !isInActiveHours(s.cfg.ActiveHours) {
		slog.Debug("heartbeat skipped: outside active hours", "agent", s.cfg.AgentID)
		return
	}

	// Check if HEARTBEAT.md is effectively empty
	if s.isHeartbeatFileEmpty() {
		slog.Debug("heartbeat skipped: HEARTBEAT.md empty", "agent", s.cfg.AgentID)
		return
	}

	// Run agent turn
	runID := fmt.Sprintf("heartbeat-%s-%d", s.cfg.AgentID, time.Now().UnixMilli())
	reply, err := s.runner(ctx, s.cfg.AgentID, s.cfg.SessionKey, s.cfg.Prompt, runID)
	if err != nil {
		slog.Warn("heartbeat agent run failed", "agent", s.cfg.AgentID, "error", err)
		return
	}

	// Normalize response: strip HEARTBEAT_OK token
	content, isOK := stripHeartbeatToken(reply, s.cfg.AckMaxChars)

	if isOK {
		slog.Debug("heartbeat OK", "agent", s.cfg.AgentID)
		return
	}

	// Dedup: skip if same content within 24h
	s.mu.Lock()
	if content == s.lastContent && time.Since(s.lastAlertAt) < 24*time.Hour {
		s.mu.Unlock()
		slog.Debug("heartbeat dedup: same content within 24h", "agent", s.cfg.AgentID)
		return
	}
	s.lastContent = content
	s.lastAlertAt = time.Now()
	s.mu.Unlock()

	// Deliver alert
	s.deliver(content)
}

// deliver sends the heartbeat alert to the configured target.
func (s *Service) deliver(content string) {
	if s.cfg.Target == "none" {
		slog.Info("heartbeat alert (target=none, not delivered)",
			"agent", s.cfg.AgentID,
			"preview", truncate(content, 100),
		)
		return
	}

	channel, chatID := s.resolveTarget()
	if channel == "" || chatID == "" {
		slog.Warn("heartbeat alert: no delivery target resolved",
			"agent", s.cfg.AgentID,
			"target", s.cfg.Target,
		)
		return
	}

	slog.Info("heartbeat alert delivered",
		"agent", s.cfg.AgentID,
		"channel", channel,
		"chatID", chatID,
		"preview", truncate(content, 100),
	)

	s.msgBus.PublishOutbound(bus.OutboundMessage{
		Channel: channel,
		ChatID:  chatID,
		Content: content,
	})
}

// resolveTarget determines where to deliver heartbeat alerts.
func (s *Service) resolveTarget() (channel, chatID string) {
	// Explicit channel target
	if s.cfg.Target != "" && s.cfg.Target != "last" && s.cfg.Target != "none" {
		channel = s.cfg.Target
		chatID = s.cfg.To
		return
	}

	// "last" — ask the resolver
	if s.lastUsed != nil {
		channel, chatID = s.lastUsed(s.cfg.AgentID)
	}

	// Override chatID if explicitly set
	if s.cfg.To != "" {
		chatID = s.cfg.To
	}

	return
}

// isHeartbeatFileEmpty checks if HEARTBEAT.md exists and has meaningful content.
// Matching TS isHeartbeatContentEffectivelyEmpty().
func (s *Service) isHeartbeatFileEmpty() bool {
	if s.cfg.Workspace == "" {
		return true
	}

	path := filepath.Join(s.cfg.Workspace, "HEARTBEAT.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return true // file doesn't exist = empty
	}

	return isEffectivelyEmpty(string(data))
}

// isEffectivelyEmpty returns true if content has no meaningful text
// (only whitespace, markdown headers, empty list items, comments).
// Matching TS isHeartbeatContentEffectivelyEmpty().
func isEffectivelyEmpty(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip markdown headers (# ...)
		if strings.HasPrefix(line, "#") {
			trimmed := strings.TrimLeft(line, "# ")
			if trimmed == "" {
				continue
			}
			// Header with text = not empty
			return false
		}
		// Skip comments (<!-- -->)
		if strings.HasPrefix(line, "<!--") {
			continue
		}
		// Skip empty list items (- , * )
		if (strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ")) && strings.TrimSpace(line[2:]) == "" {
			continue
		}
		// Any other non-empty line = meaningful content
		return false
	}
	return true
}

// stripHeartbeatToken checks for HEARTBEAT_OK in the reply.
// Returns (remaining content, isOK).
// If HEARTBEAT_OK is found at start/end and remaining content <= ackMaxChars, treat as OK.
// Matching TS stripHeartbeatToken().
func stripHeartbeatToken(reply string, ackMaxChars int) (string, bool) {
	trimmed := strings.TrimSpace(reply)

	// Exact match
	if trimmed == heartbeatOKToken {
		return "", true
	}

	// Strip common markdown/HTML wrappers
	stripped := trimmed
	for _, prefix := range []string{"**", "<b>", "<strong>", "`"} {
		suffix := strings.Replace(prefix, "<", "</", 1)
		if prefix == "**" {
			suffix = "**"
		}
		if prefix == "`" {
			suffix = "`"
		}
		stripped = strings.TrimPrefix(stripped, prefix)
		stripped = strings.TrimSuffix(stripped, suffix)
	}
	stripped = strings.TrimSpace(stripped)
	if stripped == heartbeatOKToken {
		return "", true
	}

	// Check if token appears at start or end
	hasPrefix := strings.HasPrefix(trimmed, heartbeatOKToken)
	hasSuffix := strings.HasSuffix(trimmed, heartbeatOKToken)

	if !hasPrefix && !hasSuffix {
		// Token in middle = not an ack, return full content
		return trimmed, false
	}

	remaining := trimmed
	if hasPrefix {
		remaining = strings.TrimSpace(strings.TrimPrefix(remaining, heartbeatOKToken))
	}
	if hasSuffix {
		remaining = strings.TrimSpace(strings.TrimSuffix(remaining, heartbeatOKToken))
	}

	// If remaining content is short enough, treat as OK
	if len(remaining) <= ackMaxChars {
		return "", true
	}

	// Long remaining content = not just an ack
	return remaining, false
}

// --- Active Hours ---

// isInActiveHours checks if the current time is within the configured window.
// Matching TS heartbeat-active-hours.ts.
func isInActiveHours(cfg *config.ActiveHoursConfig) bool {
	if cfg == nil || cfg.Start == "" || cfg.End == "" {
		return true // no restriction
	}

	now := time.Now()
	if cfg.Timezone != "" {
		loc, err := time.LoadLocation(cfg.Timezone)
		if err == nil {
			now = now.In(loc)
		}
	}

	startH, startM := parseHHMM(cfg.Start)
	endH, endM := parseHHMM(cfg.End)

	currentMin := now.Hour()*60 + now.Minute()
	startMin := startH*60 + startM
	endMin := endH*60 + endM

	if startMin <= endMin {
		// Normal range: e.g. 08:00 - 22:00
		return currentMin >= startMin && currentMin < endMin
	}
	// Wrap-around: e.g. 22:00 - 06:00
	return currentMin >= startMin || currentMin < endMin
}

// parseHHMM parses "HH:MM" into hours and minutes.
func parseHHMM(s string) (int, int) {
	var h, m int
	fmt.Sscanf(s, "%d:%d", &h, &m)
	return h, m
}

// truncate returns the first n characters of s.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
