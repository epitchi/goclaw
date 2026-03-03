package tts

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// Manager orchestrates TTS providers and auto-apply logic.
// Matching TS src/tts/tts.ts maybeApplyTtsToPayload().
type Manager struct {
	providers map[string]Provider
	primary   string   // primary provider name
	auto      AutoMode // auto-apply mode
	mode      Mode     // "final" or "all"
	maxLength int      // max text length before truncation (default 1500)
	timeoutMs int      // provider timeout (default 30000)
}

// ManagerConfig configures the TTS manager.
type ManagerConfig struct {
	Primary   string   // primary provider name
	Auto      AutoMode // auto-apply mode (default "off")
	Mode      Mode     // "final" or "all" (default "final")
	MaxLength int      // default 1500
	TimeoutMs int      // default 30000
}

// NewManager creates a TTS manager.
func NewManager(cfg ManagerConfig) *Manager {
	m := &Manager{
		providers: make(map[string]Provider),
		primary:   cfg.Primary,
		auto:      cfg.Auto,
		mode:      cfg.Mode,
		maxLength: cfg.MaxLength,
		timeoutMs: cfg.TimeoutMs,
	}
	if m.auto == "" {
		m.auto = AutoOff
	}
	if m.mode == "" {
		m.mode = ModeFinal
	}
	if m.maxLength <= 0 {
		m.maxLength = 1500
	}
	if m.timeoutMs <= 0 {
		m.timeoutMs = 30000
	}
	return m
}

// RegisterProvider adds a TTS provider.
func (m *Manager) RegisterProvider(p Provider) {
	m.providers[p.Name()] = p
	// If no primary set, use first registered
	if m.primary == "" {
		m.primary = p.Name()
	}
}

// GetProvider returns a provider by name.
func (m *Manager) GetProvider(name string) (Provider, bool) {
	p, ok := m.providers[name]
	return p, ok
}

// PrimaryProvider returns the primary provider name.
func (m *Manager) PrimaryProvider() string { return m.primary }

// AutoMode returns the current auto-apply mode.
func (m *Manager) AutoMode() AutoMode { return m.auto }

// Synthesize converts text to audio using the specified or primary provider.
func (m *Manager) Synthesize(ctx context.Context, text string, opts Options) (*SynthResult, error) {
	providerName := m.primary
	if opts.Voice != "" || opts.Model != "" {
		// Opts might imply a specific provider — stick with primary for now
	}

	p, ok := m.providers[providerName]
	if !ok {
		return nil, fmt.Errorf("tts provider not found: %s", providerName)
	}

	return p.Synthesize(ctx, text, opts)
}

// SynthesizeWithFallback tries the primary provider, then falls back to others.
// Matching TS resolveTtsProviderOrder().
func (m *Manager) SynthesizeWithFallback(ctx context.Context, text string, opts Options) (*SynthResult, error) {
	// Try primary first
	if p, ok := m.providers[m.primary]; ok {
		result, err := p.Synthesize(ctx, text, opts)
		if err == nil {
			return result, nil
		}
		slog.Warn("tts primary provider failed, trying fallback", "provider", m.primary, "error", err)
	}

	// Try other providers
	for name, p := range m.providers {
		if name == m.primary {
			continue
		}
		result, err := p.Synthesize(ctx, text, opts)
		if err == nil {
			slog.Info("tts fallback succeeded", "provider", name)
			return result, nil
		}
		slog.Warn("tts fallback provider failed", "provider", name, "error", err)
	}

	return nil, fmt.Errorf("all tts providers failed")
}

// MaybeApply checks auto-mode and conditionally applies TTS to a reply text.
// Returns (audioBytes, extension, applied). If not applied, returns (nil, "", false).
// Matching TS maybeApplyTtsToPayload().
//
// Parameters:
//   - text: the reply text to potentially convert
//   - channel: origin channel (affects output format, e.g. "telegram" → opus)
//   - isVoiceInbound: whether the user's message was audio/voice
//   - kind: "tool", "block", or "final"
func (m *Manager) MaybeApply(ctx context.Context, text, channel string, isVoiceInbound bool, kind string) (*SynthResult, bool) {
	if m.auto == AutoOff {
		return nil, false
	}

	// Mode filter: "final" mode skips tool/block
	if m.mode == ModeFinal && (kind == "tool" || kind == "block") {
		return nil, false
	}

	// Auto-mode check
	switch m.auto {
	case AutoInbound:
		if !isVoiceInbound {
			return nil, false
		}
	case AutoTagged:
		if !strings.Contains(text, "[[tts]]") && !strings.Contains(text, "[[tts:") {
			return nil, false
		}
	case AutoAlways:
		// Always apply
	default:
		return nil, false
	}

	// Content validation (matching TS checks)
	cleanText := stripMarkdown(text)
	cleanText = stripTtsDirectives(cleanText)
	cleanText = strings.TrimSpace(cleanText)

	if len(cleanText) < 10 {
		return nil, false
	}
	if strings.Contains(cleanText, "MEDIA:") {
		return nil, false
	}

	// Truncate if over max length
	if len(cleanText) > m.maxLength {
		cleanText = cleanText[:m.maxLength] + "..."
	}

	// Determine format based on channel
	opts := Options{}
	if channel == "telegram" {
		opts.Format = "opus" // Telegram voice bubbles need opus
	}

	result, err := m.SynthesizeWithFallback(ctx, cleanText, opts)
	if err != nil {
		slog.Warn("tts auto-apply failed", "error", err)
		return nil, false
	}

	return result, true
}

// HasProviders returns true if at least one provider is registered.
func (m *Manager) HasProviders() bool {
	return len(m.providers) > 0
}

// --- Text processing helpers ---

// stripMarkdown removes common markdown formatting for cleaner TTS input.
func stripMarkdown(text string) string {
	// Remove code blocks entirely
	text = regexp.MustCompile("(?s)```[^`]*```").ReplaceAllString(text, "")
	// Remove inline code
	text = regexp.MustCompile("`([^`]+)`").ReplaceAllString(text, "$1")
	// Bold/italic → content only
	text = regexp.MustCompile("\\*\\*([^*]+)\\*\\*").ReplaceAllString(text, "$1")
	text = regexp.MustCompile("\\*([^*]+)\\*").ReplaceAllString(text, "$1")
	text = regexp.MustCompile("__([^_]+)__").ReplaceAllString(text, "$1")
	text = regexp.MustCompile("_([^_]+)_").ReplaceAllString(text, "$1")
	// Links → text only
	text = regexp.MustCompile("\\[([^\\]]+)\\]\\([^)]+\\)").ReplaceAllString(text, "$1")
	// Headers → content
	text = regexp.MustCompile("(?m)^#+\\s+").ReplaceAllString(text, "")
	return text
}

// stripTtsDirectives removes [[tts...]] directives from text.
// Matching TS parseTtsDirectives().
func stripTtsDirectives(text string) string {
	// Remove [[tts:text]]...[[/tts:text]] blocks (keep inner text)
	text = regexp.MustCompile(`(?s)\[\[tts:text\]\](.*?)\[\[/tts:text\]\]`).ReplaceAllString(text, "$1")
	// Remove [[tts]] and [[tts:...]] tags
	text = regexp.MustCompile(`\[\[tts(?::[^\]]*)?\]\]`).ReplaceAllString(text, "")
	return text
}
