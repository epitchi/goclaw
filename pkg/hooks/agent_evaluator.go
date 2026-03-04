package hooks

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AgentDelegateFunc is the callback for delegating to a reviewer agent.
// Injected from cmd layer to avoid hooks → tools import cycle.
type AgentDelegateFunc func(ctx context.Context, agentKey, task string) (string, error)

// AgentEvaluator delegates to a reviewer agent for quality validation.
type AgentEvaluator struct {
	delegateFunc AgentDelegateFunc
}

// NewAgentEvaluator creates an agent evaluator with the given delegate callback.
func NewAgentEvaluator(delegateFunc AgentDelegateFunc) *AgentEvaluator {
	return &AgentEvaluator{delegateFunc: delegateFunc}
}

func (ae *AgentEvaluator) Evaluate(ctx context.Context, hook HookConfig, hctx HookContext) (*HookResult, error) {
	if hook.Agent == "" {
		return nil, fmt.Errorf("agent hook has empty agent key")
	}

	timeout := hook.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultTimeoutSeconds
	}

	evalCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	// Skip hooks for the evaluation delegation itself (prevent recursion)
	evalCtx = WithSkipHooks(evalCtx, true)

	prompt := buildEvalPrompt(hctx)
	response, err := ae.delegateFunc(evalCtx, hook.Agent, prompt)
	if err != nil {
		return nil, fmt.Errorf("agent evaluation failed: %w", err)
	}

	return parseEvalResponse(response), nil
}

func buildEvalPrompt(hctx HookContext) string {
	return fmt.Sprintf(
		"[Quality Gate Evaluation]\n"+
			"You are reviewing the output of a delegated task for quality.\n\n"+
			"Original task: %s\n"+
			"Source agent: %s\n"+
			"Target agent: %s\n\n"+
			"Output to evaluate:\n%s\n\n"+
			"Respond with EXACTLY one of:\n"+
			"- \"APPROVED\" if the output meets quality standards (optionally followed by comments)\n"+
			"- \"REJECTED: <specific feedback>\" with actionable improvement suggestions",
		hctx.Task, hctx.SourceAgentKey, hctx.TargetAgentKey, hctx.Content)
}

func parseEvalResponse(response string) *HookResult {
	upper := strings.ToUpper(strings.TrimSpace(response))

	if strings.HasPrefix(upper, "APPROVED") {
		return &HookResult{Passed: true}
	}

	// Extract feedback from "REJECTED: <feedback>" format
	feedback := response
	if idx := strings.Index(upper, "REJECTED:"); idx >= 0 {
		feedback = strings.TrimSpace(response[idx+len("REJECTED:"):])
	}

	return &HookResult{Passed: false, Feedback: feedback}
}
