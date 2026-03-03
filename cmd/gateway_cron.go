package cmd

import (
	"context"
	"fmt"

	"github.com/nextlevelbuilder/goclaw/pkg/agent"
	"github.com/nextlevelbuilder/goclaw/pkg/bus"
	"github.com/nextlevelbuilder/goclaw/pkg/config"
	"github.com/nextlevelbuilder/goclaw/pkg/scheduler"
	"github.com/nextlevelbuilder/goclaw/pkg/sessions"
	"github.com/nextlevelbuilder/goclaw/pkg/store"
)

// makeCronJobHandler creates a cron job handler that routes through the scheduler's cron lane.
// This ensures per-session concurrency control (same job can't run concurrently)
// and integration with /stop, /stopall commands.
func makeCronJobHandler(sched *scheduler.Scheduler, msgBus *bus.MessageBus, cfg *config.Config) func(job *store.CronJob) (*store.CronJobResult, error) {
	return func(job *store.CronJob) (*store.CronJobResult, error) {
		agentID := job.AgentID
		if agentID == "" {
			agentID = cfg.ResolveDefaultAgentID()
		} else {
			agentID = config.NormalizeAgentID(agentID)
		}

		sessionKey := sessions.BuildCronSessionKey(agentID, job.ID)
		channel := job.Payload.Channel
		if channel == "" {
			channel = "cron"
		}

		// Schedule through cron lane — scheduler handles agent resolution and concurrency
		outCh := sched.Schedule(context.Background(), scheduler.LaneCron, agent.RunRequest{
			SessionKey: sessionKey,
			Message:    job.Payload.Message,
			Channel:    channel,
			ChatID:     job.Payload.To,
			UserID:     job.UserID,
			RunID:      fmt.Sprintf("cron:%s", job.ID),
			Stream:     false,
			TraceName:  fmt.Sprintf("Cron [%s] - %s", job.Name, agentID),
			TraceTags:  []string{"cron"},
		})

		// Block until the scheduled run completes
		outcome := <-outCh
		if outcome.Err != nil {
			return nil, outcome.Err
		}

		result := outcome.Result

		// If job wants delivery to a channel, publish outbound
		if job.Payload.Deliver && job.Payload.Channel != "" && job.Payload.To != "" {
			msgBus.PublishOutbound(bus.OutboundMessage{
				Channel: job.Payload.Channel,
				ChatID:  job.Payload.To,
				Content: result.Content,
			})
		}

		cronResult := &store.CronJobResult{
			Content: result.Content,
		}
		if result.Usage != nil {
			cronResult.InputTokens = result.Usage.PromptTokens
			cronResult.OutputTokens = result.Usage.CompletionTokens
		}

		return cronResult, nil
	}
}
