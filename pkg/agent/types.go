package agent

import "context"

// Agent is the core abstraction for an AI agent execution loop.
// Implemented by *Loop; extracted as an interface for testability and composability.
type Agent interface {
	ID() string
	Run(ctx context.Context, req RunRequest) (*RunResult, error)
	IsRunning() bool
	Model() string
}
