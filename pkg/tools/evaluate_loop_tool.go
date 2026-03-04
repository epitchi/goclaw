package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/epitchi/goclaw/pkg/hooks"
)

const (
	defaultMaxRounds = 3
	maxAllowedRounds = 5
)

// EvaluateLoopTool orchestrates a generator-evaluator feedback loop.
// Agent A generates output, Agent B evaluates it, loop until quality threshold is met.
type EvaluateLoopTool struct {
	manager *DelegateManager
}

func NewEvaluateLoopTool(manager *DelegateManager) *EvaluateLoopTool {
	return &EvaluateLoopTool{manager: manager}
}

func (t *EvaluateLoopTool) Name() string { return "evaluate_loop" }

func (t *EvaluateLoopTool) Description() string {
	return "Run a generate-evaluate-revise loop between two agents. " +
		"Generator produces output, evaluator approves or rejects with feedback, " +
		"generator revises until approved or max rounds reached."
}

func (t *EvaluateLoopTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"generator": map[string]interface{}{
				"type":        "string",
				"description": "Agent key for the content generator",
			},
			"evaluator": map[string]interface{}{
				"type":        "string",
				"description": "Agent key for the quality evaluator",
			},
			"task": map[string]interface{}{
				"type":        "string",
				"description": "Initial task for the generator",
			},
			"max_rounds": map[string]interface{}{
				"type":        "number",
				"description": "Maximum generate-evaluate rounds (default 3, max 5)",
			},
			"pass_criteria": map[string]interface{}{
				"type":        "string",
				"description": "Criteria the evaluator uses to approve/reject output",
			},
			"context": map[string]interface{}{
				"type":        "string",
				"description": "Optional additional context for both agents",
			},
			"team_task_id": map[string]interface{}{
				"type":        "string",
				"description": "Optional team task ID for auto-completion on success",
			},
		},
		"required": []string{"generator", "evaluator", "task"},
	}
}

func (t *EvaluateLoopTool) Execute(ctx context.Context, args map[string]interface{}) *Result {
	generatorKey, _ := args["generator"].(string)
	evaluatorKey, _ := args["evaluator"].(string)
	task, _ := args["task"].(string)

	if generatorKey == "" || evaluatorKey == "" || task == "" {
		return ErrorResult("generator, evaluator, and task are required")
	}

	maxRounds := defaultMaxRounds
	if v, ok := args["max_rounds"].(float64); ok && int(v) > 0 {
		maxRounds = int(v)
		if maxRounds > maxAllowedRounds {
			maxRounds = maxAllowedRounds
		}
	}

	passCriteria, _ := args["pass_criteria"].(string)
	extraContext, _ := args["context"].(string)

	var teamTaskID uuid.UUID
	if v, _ := args["team_task_id"].(string); v != "" {
		teamTaskID, _ = uuid.Parse(v)
	}

	// Skip quality gates for all internal delegations (prevent recursion).
	loopCtx := hooks.WithSkipHooks(ctx, true)

	var lastOutput string
	var lastFeedback string

	for round := 1; round <= maxRounds; round++ {
		// --- Generate ---
		genTask := task
		if extraContext != "" {
			genTask = fmt.Sprintf("[Additional Context]\n%s\n\n[Task]\n%s", extraContext, task)
		}
		if round > 1 && lastFeedback != "" {
			genTask = fmt.Sprintf(
				"[Revision — Round %d/%d]\n"+
					"Your previous output was reviewed and needs improvement.\n\n"+
					"Original task: %s\n"+
					"Evaluator feedback: %s\n\n"+
					"Please revise your output addressing all feedback points.",
				round, maxRounds, task, lastFeedback)
			if extraContext != "" {
				genTask = fmt.Sprintf("[Additional Context]\n%s\n\n%s", extraContext, genTask)
			}
		}

		genResult, err := t.manager.Delegate(loopCtx, DelegateOpts{
			TargetAgentKey: generatorKey,
			Task:           genTask,
			Mode:           "sync",
		})
		if err != nil {
			return ErrorResult(fmt.Sprintf("generator %q failed in round %d: %s", generatorKey, round, err))
		}
		lastOutput = genResult.Content

		// --- Evaluate ---
		evalPrompt := buildEvalLoopPrompt(lastOutput, passCriteria, round, maxRounds)
		evalResult, err := t.manager.Delegate(loopCtx, DelegateOpts{
			TargetAgentKey: evaluatorKey,
			Task:           evalPrompt,
			Mode:           "sync",
		})
		if err != nil {
			return ErrorResult(fmt.Sprintf("evaluator %q failed in round %d: %s", evaluatorKey, round, err))
		}

		// Check approval
		if isApproved(evalResult.Content) {
			// Auto-complete team task on the final successful round.
			if teamTaskID != uuid.Nil && t.manager.teamStore != nil {
				if teamTask, getErr := t.manager.teamStore.GetTask(ctx, teamTaskID); getErr == nil {
					_ = t.manager.teamStore.ClaimTask(ctx, teamTaskID, uuid.Nil, teamTask.TeamID)
					_ = t.manager.teamStore.CompleteTask(ctx, teamTaskID, teamTask.TeamID, lastOutput)
				}
			}

			return NewResult(fmt.Sprintf(
				"Evaluate-optimize loop completed in %d round(s).\n"+
					"Generator: %s | Evaluator: %s\n\n"+
					"Final output:\n%s",
				round, generatorKey, evaluatorKey, lastOutput))
		}

		// Extract feedback for next round
		lastFeedback = extractFeedback(evalResult.Content)
	}

	// Max rounds exceeded
	return NewResult(fmt.Sprintf(
		"Evaluate-optimize loop reached max rounds (%d) without evaluator approval.\n\n"+
			"Last evaluator feedback: %s\n\n"+
			"Last generator output:\n%s",
		maxRounds, lastFeedback, lastOutput))
}

func buildEvalLoopPrompt(output, criteria string, round, maxRounds int) string {
	criteriaSection := ""
	if criteria != "" {
		criteriaSection = fmt.Sprintf("\nCriteria: %s\n", criteria)
	}

	return fmt.Sprintf(
		"[Quality Evaluation — Round %d/%d]\n"+
			"Evaluate this output against the criteria below.\n"+
			"%s\n"+
			"Output to evaluate:\n%s\n\n"+
			"Respond with EXACTLY one of:\n"+
			"- \"APPROVED\" if the output meets ALL criteria (optionally followed by comments)\n"+
			"- \"REJECTED: <specific feedback>\" with actionable improvement suggestions",
		round, maxRounds, criteriaSection, output)
}

func isApproved(response string) bool {
	upper := strings.ToUpper(strings.TrimSpace(response))
	return strings.HasPrefix(upper, "APPROVED")
}

func extractFeedback(response string) string {
	upper := strings.ToUpper(response)
	if idx := strings.Index(upper, "REJECTED:"); idx >= 0 {
		return strings.TrimSpace(response[idx+len("REJECTED:"):])
	}
	return response
}
