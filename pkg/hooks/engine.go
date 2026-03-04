package hooks

import (
	"context"
	"fmt"
	"log/slog"
)

// Engine orchestrates hook evaluation for a set of events.
type Engine struct {
	evaluators map[HookType]HookEvaluator
}

// NewEngine creates a new hook engine.
func NewEngine() *Engine {
	return &Engine{evaluators: make(map[HookType]HookEvaluator)}
}

// RegisterEvaluator adds a hook type evaluator.
func (e *Engine) RegisterEvaluator(hookType HookType, eval HookEvaluator) {
	e.evaluators[hookType] = eval
}

// EvaluateHooks runs all hooks matching the given event.
// Returns the first blocking failure. Non-blocking failures are logged but don't stop evaluation.
// If all hooks pass (or none match), returns a passing result.
func (e *Engine) EvaluateHooks(ctx context.Context, hooks []HookConfig, event string, hctx HookContext) (*HookResult, error) {
	for _, hook := range hooks {
		if hook.Event != event {
			continue
		}

		eval, ok := e.evaluators[hook.Type]
		if !ok {
			slog.Warn("hooks: unknown hook type, skipping", "type", hook.Type, "event", event)
			continue
		}

		result, err := eval.Evaluate(ctx, hook, hctx)
		if err != nil {
			slog.Warn("hooks: evaluator error, skipping",
				"type", hook.Type, "event", event, "error", err)
			continue
		}

		if result.Passed {
			slog.Info("hooks: gate passed", "type", hook.Type, "event", event)
			continue
		}

		// Hook failed
		if hook.BlockOnFailure {
			slog.Warn("hooks: blocking gate failed",
				"type", hook.Type, "event", event, "feedback", truncate(result.Feedback, 200))
			return result, nil
		}

		slog.Warn("hooks: non-blocking gate failed",
			"type", hook.Type, "event", event, "feedback", truncate(result.Feedback, 200))
	}

	return &HookResult{Passed: true}, nil
}

// EvaluateSingleHook evaluates a single hook config against a context.
// Convenience method for retry loops that need to re-evaluate one gate at a time.
func (e *Engine) EvaluateSingleHook(ctx context.Context, hook HookConfig, hctx HookContext) (*HookResult, error) {
	eval, ok := e.evaluators[hook.Type]
	if !ok {
		return nil, fmt.Errorf("unknown hook type: %s", hook.Type)
	}
	return eval.Evaluate(ctx, hook, hctx)
}

func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "..."
}
