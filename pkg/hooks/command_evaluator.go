package hooks

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const defaultTimeoutSeconds = 60

// CommandEvaluator runs a shell command to validate output.
// Exit 0 = pass, non-zero = fail. Stderr content is used as feedback.
type CommandEvaluator struct {
	workspace string // working directory for command execution
}

// NewCommandEvaluator creates a command evaluator with the given workspace directory.
func NewCommandEvaluator(workspace string) *CommandEvaluator {
	return &CommandEvaluator{workspace: workspace}
}

func (ce *CommandEvaluator) Evaluate(ctx context.Context, hook HookConfig, hctx HookContext) (*HookResult, error) {
	if hook.Command == "" {
		return nil, fmt.Errorf("command hook has empty command")
	}

	timeout := hook.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultTimeoutSeconds
	}

	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", hook.Command)
	cmd.Dir = ce.workspace

	// Pass content via stdin
	cmd.Stdin = strings.NewReader(hctx.Content)

	// Set environment variables
	cmd.Env = append(cmd.Environ(),
		"HOOK_EVENT="+hctx.Event,
		"HOOK_SOURCE_AGENT="+hctx.SourceAgentKey,
		"HOOK_TARGET_AGENT="+hctx.TargetAgentKey,
		"HOOK_TASK="+hctx.Task,
		"HOOK_USER_ID="+hctx.UserID,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		// Exit 0 = pass
		return &HookResult{Passed: true}, nil
	}

	// Non-zero exit = fail
	feedback := strings.TrimSpace(stderr.String())
	if feedback == "" {
		feedback = fmt.Sprintf("command %q exited with error: %v", hook.Command, err)
	}

	return &HookResult{Passed: false, Feedback: feedback}, nil
}
