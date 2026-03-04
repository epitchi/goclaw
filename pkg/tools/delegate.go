package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/epitchi/goclaw/pkg/bus"
	"github.com/epitchi/goclaw/pkg/hooks"
	"github.com/epitchi/goclaw/pkg/store"
	"github.com/epitchi/goclaw/pkg/tracing"
)

const defaultMaxDelegationLoad = 5

// DelegationTask tracks an active delegation for concurrency control and cancellation.
type DelegationTask struct {
	ID             string     `json:"id"`
	SourceAgentID  uuid.UUID  `json:"source_agent_id"`
	SourceAgentKey string     `json:"source_agent_key"`
	TargetAgentID  uuid.UUID  `json:"target_agent_id"`
	TargetAgentKey string     `json:"target_agent_key"`
	UserID         string     `json:"user_id"`
	Task           string     `json:"task"`
	Status         string     `json:"status"` // "running", "completed", "failed", "cancelled"
	Mode           string     `json:"mode"`   // "sync" or "async"
	SessionKey     string     `json:"session_key"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`

	// Origin metadata for async announce routing
	OriginChannel  string `json:"-"`
	OriginChatID   string `json:"-"`
	OriginPeerKind string `json:"-"`
	OriginLocalKey string `json:"-"` // composite key with topic/thread suffix for routing

	// Trace context for announce linking (same pattern as SubagentTask)
	OriginTraceID    uuid.UUID `json:"-"`
	OriginRootSpanID uuid.UUID `json:"-"`

	// Team tracking
	TeamID     uuid.UUID `json:"-"` // from link.TeamID (for delegation history)
	TeamTaskID uuid.UUID `json:"-"`

	cancelFunc context.CancelFunc `json:"-"`
}

// DelegateOpts configures a single delegation call.
type DelegateOpts struct {
	TargetAgentKey string
	Task           string
	Context        string    // optional extra context
	Mode           string    // "sync" (default) or "async"
	TeamTaskID     uuid.UUID // optional: auto-complete this team task on success
}

// DelegateRunRequest is the request passed to the AgentRunFunc callback.
// Mirrors agent.RunRequest without importing the agent package (avoids import cycle).
type DelegateRunRequest struct {
	SessionKey        string
	Message           string
	UserID            string
	Channel           string
	ChatID            string
	PeerKind          string
	RunID             string
	Stream            bool
	ExtraSystemPrompt string
	MaxIterations     int // per-delegation override (0 = use agent default)
}

// DelegateRunResult is the result from AgentRunFunc.
type DelegateRunResult struct {
	Content      string
	Iterations   int
	MediaPaths   []string // media file paths from tool results (e.g. generated images)
	Deliverables []string // actual content from tool outputs (e.g. written file text, image prompt)
}

// DelegateArtifacts holds forwarded artifacts from delegation results.
// Used to accumulate artifacts from intermediate completions until the final
// announce fires. New artifact types (files, voice, etc.) should be added here.
type DelegateArtifacts struct {
	Media   []string                // file paths to forward (images, documents, audio, etc.)
	Results []DelegateResultSummary // result summaries from completed delegations
}

// DelegateResultSummary is a compact representation of a delegation result
// included in the final announce so the lead has all results in one message.
type DelegateResultSummary struct {
	AgentKey     string
	Content      string
	HasMedia     bool
	Deliverables []string // actual content from tool outputs
}

// AgentRunFunc runs an agent by key with the given request.
// This callback is injected from the cmd layer to avoid tools→agent import cycle.
type AgentRunFunc func(ctx context.Context, agentKey string, req DelegateRunRequest) (*DelegateRunResult, error)

// DelegateResult is the outcome of a delegation.
type DelegateResult struct {
	Content      string
	Iterations   int
	DelegationID string   // for async: the delegation ID to track/cancel
	MediaPaths   []string // media file paths from delegation result
}

// linkSettings holds per-user restriction rules from agent_links.settings JSONB.
// NOTE: This is NOT the same as other_config.description (summoning prompt).
type linkSettings struct {
	RequireRole string   `json:"require_role"`
	UserAllow   []string `json:"user_allow"`
	UserDeny    []string `json:"user_deny"`
}

// DelegateManager manages inter-agent delegation lifecycle.
// Similar to SubagentManager but delegates to fully-configured named agents.
type DelegateManager struct {
	runAgent     AgentRunFunc
	linkStore    store.AgentLinkStore
	agentStore   store.AgentStore
	teamStore    store.TeamStore     // optional: enables auto-complete of team tasks
	sessionStore store.SessionStore  // optional: enables session cleanup
	msgBus       *bus.MessageBus     // for event broadcast + async announce (PublishInbound)
	hookEngine   *hooks.Engine       // optional: quality gate evaluation

	active            sync.Map // delegationID → *DelegationTask
	pendingArtifacts  sync.Map // sourceAgentID string → *DelegateArtifacts
	completedMu       sync.Mutex
	completedSessions []string // session keys pending cleanup
}

// NewDelegateManager creates a new delegation manager.
func NewDelegateManager(
	runAgent AgentRunFunc,
	linkStore store.AgentLinkStore,
	agentStore store.AgentStore,
	msgBus *bus.MessageBus,
) *DelegateManager {
	return &DelegateManager{
		runAgent:   runAgent,
		linkStore:  linkStore,
		agentStore: agentStore,
		msgBus:     msgBus,
	}
}

// SetTeamStore enables auto-completion of team tasks on delegation success.
func (dm *DelegateManager) SetTeamStore(ts store.TeamStore) {
	dm.teamStore = ts
}

// SetSessionStore enables session cleanup after team tasks complete.
func (dm *DelegateManager) SetSessionStore(ss store.SessionStore) {
	dm.sessionStore = ss
}

// SetHookEngine enables quality gate evaluation on delegation results.
func (dm *DelegateManager) SetHookEngine(engine *hooks.Engine) {
	dm.hookEngine = engine
}

// Delegate executes a synchronous delegation to another agent.
func (dm *DelegateManager) Delegate(ctx context.Context, opts DelegateOpts) (*DelegateResult, error) {
	task, _, err := dm.prepareDelegation(ctx, opts, "sync")
	if err != nil {
		return nil, err
	}

	dm.active.Store(task.ID, task)
	defer func() {
		now := time.Now()
		task.CompletedAt = &now
		dm.active.Delete(task.ID)
	}()

	message := buildDelegateMessage(opts)
	dm.emitEvent("delegation.started", task)
	slog.Info("delegation started", "id", task.ID, "target", opts.TargetAgentKey, "mode", "sync")

	// Propagate parent trace ID so the delegate trace links back.
	// Clear senderID — delegations are system-initiated, the delegate agent
	// should not inherit the caller's group writer permissions (the delegate
	// agent has its own writer list, and would incorrectly deny writes).
	delegateCtx := store.WithSenderID(ctx, "")
	if parentTraceID := tracing.TraceIDFromContext(ctx); parentTraceID != uuid.Nil {
		delegateCtx = tracing.WithDelegateParentTraceID(delegateCtx, parentTraceID)
	}

	startTime := time.Now()
	result, err := dm.runAgent(delegateCtx, opts.TargetAgentKey, dm.buildRunRequest(task, message))
	duration := time.Since(startTime)
	if err != nil {
		task.Status = "failed"
		dm.emitEvent("delegation.failed", task)
		dm.saveDelegationHistory(task, "", err, duration)
		return nil, fmt.Errorf("delegation to %q failed: %w", opts.TargetAgentKey, err)
	}

	// Apply quality gates before marking completed.
	if result, err = dm.applyQualityGates(delegateCtx, task, opts, result); err != nil {
		task.Status = "failed"
		dm.emitEvent("delegation.failed", task)
		dm.saveDelegationHistory(task, "", err, duration)
		return nil, fmt.Errorf("delegation to %q failed quality gate: %w", opts.TargetAgentKey, err)
	}

	task.Status = "completed"
	dm.emitEvent("delegation.completed", task)
	dm.trackCompleted(task)
	dm.autoCompleteTeamTask(task, result.Content, result.Deliverables)
	dm.saveDelegationHistory(task, result.Content, nil, duration)
	slog.Info("delegation completed", "id", task.ID, "target", opts.TargetAgentKey, "iterations", result.Iterations)

	return &DelegateResult{Content: result.Content, Iterations: result.Iterations, DelegationID: task.ID, MediaPaths: result.MediaPaths}, nil
}

// DelegateAsync spawns a delegation in the background and announces the result back.
func (dm *DelegateManager) DelegateAsync(ctx context.Context, opts DelegateOpts) (*DelegateResult, error) {
	task, _, err := dm.prepareDelegation(ctx, opts, "async")
	if err != nil {
		return nil, err
	}

	taskCtx, taskCancel := context.WithCancel(context.Background())
	task.cancelFunc = taskCancel
	dm.active.Store(task.ID, task)

	// Capture parent trace ID before goroutine (ctx.Background() loses it)
	parentTraceID := tracing.TraceIDFromContext(ctx)
	if parentTraceID != uuid.Nil {
		taskCtx = tracing.WithDelegateParentTraceID(taskCtx, parentTraceID)
	}

	message := buildDelegateMessage(opts)
	dm.emitEvent("delegation.started", task)
	slog.Info("delegation started (async)", "id", task.ID, "target", opts.TargetAgentKey)

	runReq := dm.buildRunRequest(task, message)

	go func() {
		defer func() {
			now := time.Now()
			task.CompletedAt = &now
			dm.active.Delete(task.ID)
		}()

		startTime := time.Now()
		result, runErr := dm.runAgent(taskCtx, opts.TargetAgentKey, runReq)
		duration := time.Since(startTime)

		// Count sibling delegations still running (exclude self)
		siblings := dm.ListActive(task.SourceAgentID)
		siblingCount := 0
		for _, s := range siblings {
			if s.ID != task.ID {
				siblingCount++
			}
		}
		// Announce result to parent via message bus
		if dm.msgBus != nil && task.OriginChannel != "" {
			elapsed := time.Since(task.CreatedAt)

			if siblingCount > 0 {
				// Intermediate completion: accumulate artifacts + result summary.
				// The final announce includes all sibling results so the lead doesn't
				// need to call team_tasks to aggregate.
				arts := &DelegateArtifacts{}
				if result != nil {
					arts.Media = result.MediaPaths
					arts.Results = []DelegateResultSummary{{
						AgentKey:     task.TargetAgentKey,
						Content:      result.Content,
						HasMedia:     len(result.MediaPaths) > 0,
						Deliverables: result.Deliverables,
					}}
				} else if runErr != nil {
					arts.Results = []DelegateResultSummary{{
						AgentKey: task.TargetAgentKey,
						Content:  fmt.Sprintf("[failed] %s", runErr.Error()),
					}}
				}
				dm.accumulateArtifacts(task.SourceAgentID, arts)
				slog.Info("delegation announce suppressed (siblings still running)",
					"id", task.ID, "target", task.TargetAgentKey, "siblings", siblingCount)
			} else {
				// Last completion: collect all accumulated artifacts + own result
				artifacts := dm.collectArtifacts(task.SourceAgentID)
				if result != nil {
					artifacts.Media = append(artifacts.Media, result.MediaPaths...)
					artifacts.Results = append(artifacts.Results, DelegateResultSummary{
						AgentKey:     task.TargetAgentKey,
						Content:      result.Content,
						HasMedia:     len(result.MediaPaths) > 0,
						Deliverables: result.Deliverables,
					})
				}

				announceMeta := map[string]string{
					"origin_channel":      task.OriginChannel,
					"origin_peer_kind":    task.OriginPeerKind,
					"parent_agent":        task.SourceAgentKey,
					"delegation_id":       task.ID,
					"target_agent":        task.TargetAgentKey,
					"origin_trace_id":     task.OriginTraceID.String(),
					"origin_root_span_id": task.OriginRootSpanID.String(),
				}
				if task.OriginLocalKey != "" {
					announceMeta["origin_local_key"] = task.OriginLocalKey
				}
				announceMsg := bus.InboundMessage{
					Channel:  "system",
					SenderID: fmt.Sprintf("delegate:%s", task.ID),
					ChatID:   task.OriginChatID,
					Content:  formatDelegateAnnounce(task, artifacts, runErr, elapsed),
					UserID:   task.UserID,
					Metadata: announceMeta,
					Media:    artifacts.Media,
				}
				dm.msgBus.PublishInbound(announceMsg)
			}
		}

		if runErr != nil {
			task.Status = "failed"
			dm.emitEvent("delegation.failed", task)
			dm.saveDelegationHistory(task, "", runErr, duration)
		} else {
			// Apply quality gates before marking completed.
			if result, runErr = dm.applyQualityGates(taskCtx, task, opts, result); runErr != nil {
				task.Status = "failed"
				dm.emitEvent("delegation.failed", task)
				dm.saveDelegationHistory(task, "", runErr, duration)
			} else {
				task.Status = "completed"
				dm.emitEvent("delegation.completed", task)
				dm.trackCompleted(task)
				resultContent := ""
				var deliverables []string
				if result != nil {
					resultContent = result.Content
					deliverables = result.Deliverables
				}
				// Auto-complete the team task for EVERY delegation (not just the last one).
				// Each delegation has its own TeamTaskID — the isLastDelegation guard
				// is for announce batching only, not for task completion.
				dm.autoCompleteTeamTask(task, resultContent, deliverables)
				dm.saveDelegationHistory(task, resultContent, nil, duration)
			}
		}
		slog.Info("delegation finished (async)", "id", task.ID, "target", task.TargetAgentKey, "status", task.Status)
	}()

	return &DelegateResult{DelegationID: task.ID}, nil
}

// --- internal helpers ---

func (dm *DelegateManager) prepareDelegation(ctx context.Context, opts DelegateOpts, mode string) (*DelegationTask, *store.AgentLinkData, error) {
	sourceAgentID := store.AgentIDFromContext(ctx)
	if sourceAgentID == uuid.Nil {
		return nil, nil, fmt.Errorf("delegation requires managed mode (no agent ID in context)")
	}

	sourceAgent, err := dm.agentStore.GetByID(ctx, sourceAgentID)
	if err != nil {
		return nil, nil, fmt.Errorf("source agent not found: %w", err)
	}

	targetAgent, err := dm.agentStore.GetByKey(ctx, opts.TargetAgentKey)
	if err != nil {
		return nil, nil, fmt.Errorf("target agent %q not found", opts.TargetAgentKey)
	}

	link, err := dm.linkStore.GetLinkBetween(ctx, sourceAgentID, targetAgent.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to check delegation permission: %w", err)
	}
	if link == nil {
		return nil, nil, fmt.Errorf("no delegation link from this agent to %q. Available targets are listed in AGENTS.md", opts.TargetAgentKey)
	}

	userID := store.UserIDFromContext(ctx)
	if err := checkUserPermission(link.Settings, userID); err != nil {
		return nil, nil, err
	}

	// Resolve team once — used for task enforcement, validation, and access checks.
	var team *store.TeamData
	if dm.teamStore != nil {
		team, _ = dm.teamStore.GetTeamForAgent(ctx, sourceAgentID)
	}

	// Enforce team_task_id for team members: every delegation must be tracked.
	if team != nil && opts.TeamTaskID == uuid.Nil {
		hint := dm.pendingTasksHint(ctx, team.ID)
		return nil, nil, fmt.Errorf(
			"spawn requires a valid team_task_id (UUID). "+
				"Create a task first with team_tasks action=create, "+
				"then pass the returned task id as team_task_id.%s",
			hint)
	}

	// Validate that team_task_id belongs to the agent's team (prevent cross-team task completion).
	if dm.teamStore != nil && opts.TeamTaskID != uuid.Nil {
		teamTask, err := dm.teamStore.GetTask(ctx, opts.TeamTaskID)
		if err != nil {
			hint := ""
			if team != nil {
				hint = dm.pendingTasksHint(ctx, team.ID)
			}
			return nil, nil, fmt.Errorf("team_task_id %s not found.%s", opts.TeamTaskID, hint)
		}
		if team != nil {
			if teamTask.TeamID != team.ID {
				return nil, nil, fmt.Errorf("team_task_id does not belong to your team")
			}
			userID := store.UserIDFromContext(ctx)
			channel := ToolChannelFromCtx(ctx)
			if err := checkTeamAccess(team.Settings, userID, channel); err != nil {
				return nil, nil, fmt.Errorf("team access denied: %w", err)
			}
		}

		// Auto-populate task description from spawn prompt if empty.
		// This ensures the task board has full context for audit/visibility
		// without relying on the LLM to set description at task creation time.
		if teamTask.Description == "" && opts.Task != "" {
			_ = dm.teamStore.UpdateTask(ctx, opts.TeamTaskID, map[string]any{
				"description": opts.Task,
			})
		}

		// Claim task early so status moves to in_progress immediately.
		// This prevents the pending reminder from re-triggering spawns for
		// tasks that are already running. The ClaimTask in autoCompleteTeamTask()
		// will harmlessly fail (WHERE status='pending' won't match).
		_ = dm.teamStore.ClaimTask(ctx, opts.TeamTaskID, targetAgent.ID, teamTask.TeamID)
	}

	linkCount := dm.ActiveCountForLink(sourceAgentID, targetAgent.ID)
	if link.MaxConcurrent > 0 && linkCount >= link.MaxConcurrent {
		return nil, nil, fmt.Errorf("delegation link to %q is at capacity (%d/%d active). Try again later or handle the task yourself",
			opts.TargetAgentKey, linkCount, link.MaxConcurrent)
	}

	targetCount := dm.ActiveCountForTarget(targetAgent.ID)
	maxLoad := parseMaxDelegationLoad(targetAgent.OtherConfig)
	if targetCount >= maxLoad {
		return nil, nil, fmt.Errorf("agent %q is at capacity (%d/%d active delegations). Either wait and retry, use a different agent, or handle the task yourself",
			opts.TargetAgentKey, targetCount, maxLoad)
	}

	channel := ToolChannelFromCtx(ctx)
	chatID := ToolChatIDFromCtx(ctx)
	peerKind := ToolPeerKindFromCtx(ctx)
	localKey := ToolLocalKeyFromCtx(ctx)

	delegationID := uuid.NewString()[:12]
	task := &DelegationTask{
		ID:             delegationID,
		SourceAgentID:  sourceAgentID,
		SourceAgentKey: sourceAgent.AgentKey,
		TargetAgentID:  targetAgent.ID,
		TargetAgentKey: opts.TargetAgentKey,
		UserID:         userID,
		Task:           opts.Task,
		Status:         "running",
		Mode:           mode,
		SessionKey: fmt.Sprintf("delegate:%s:%s:%s",
			sourceAgentID.String()[:8], opts.TargetAgentKey, delegationID),
		CreatedAt:        time.Now(),
		OriginChannel:    channel,
		OriginChatID:     chatID,
		OriginPeerKind:   peerKind,
		OriginLocalKey:   localKey,
		OriginTraceID:    tracing.TraceIDFromContext(ctx),
		OriginRootSpanID: tracing.ParentSpanIDFromContext(ctx),
		TeamTaskID:       opts.TeamTaskID,
	}

	// Carry team_id from the link (for delegation history filtering by team)
	if link.TeamID != nil {
		task.TeamID = *link.TeamID
	}

	return task, link, nil
}

func buildDelegateMessage(opts DelegateOpts) string {
	if opts.Context != "" {
		return fmt.Sprintf("[Additional Context]\n%s\n\n[Task]\n%s", opts.Context, opts.Task)
	}
	return opts.Task
}

// pendingTasksHint returns a formatted hint listing pending team tasks,
// so the LLM can self-correct a hallucinated team_task_id.
func (dm *DelegateManager) pendingTasksHint(ctx context.Context, teamID uuid.UUID) string {
	tasks, err := dm.teamStore.ListTasks(ctx, teamID, "newest", "", "")
	if err != nil {
		return ""
	}
	var ids []string
	for _, t := range tasks {
		if t.Status == store.TeamTaskStatusPending {
			ids = append(ids, fmt.Sprintf("%s (%s)", t.ID, t.Subject))
		}
	}
	if len(ids) == 0 {
		return ""
	}
	return fmt.Sprintf(" Pending tasks: %s", strings.Join(ids, ", "))
}

func (dm *DelegateManager) buildRunRequest(task *DelegationTask, message string) DelegateRunRequest {
	return DelegateRunRequest{
		SessionKey: task.SessionKey,
		Message:    message,
		UserID:     task.UserID,
		Channel:    "delegate",
		ChatID:     task.OriginChatID,
		PeerKind:   task.OriginPeerKind,
		RunID:      fmt.Sprintf("delegate-%s", task.ID),
		Stream:     false,
		ExtraSystemPrompt: "[Delegation Context]\nYou are handling a delegated task from another agent.\n" +
			"- Focus exclusively on the delegated task below.\n" +
			"- Your complete response will be returned to the requesting agent.\n" +
			"- Do NOT try to communicate with the end user directly.\n" +
			"- Do NOT use your persona name or self-references (e.g. do not say your name). Write factual, neutral content.\n" +
			"- Be concise and deliver actionable results.",
	}
}
