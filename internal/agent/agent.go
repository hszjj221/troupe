package agent

import (
	"context"

	"github.com/hszjj221/troupe/internal/store"
)

// Agent runs one bounded coding task and reads its result from runDir.
type Agent interface {
	Name() string
	// Run uses ctx for cancellation and timeouts, and archives logs in runDir.
	Run(ctx context.Context, task store.Task, runDir string) (store.Result, error)
}
