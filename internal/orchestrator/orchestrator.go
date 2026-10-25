package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/hszjj221/troupe/internal/agent"
	"github.com/hszjj221/troupe/internal/store"
)

// Orchestrator executes tasks one at a time. A store and run root must be owned
// by one orchestrator; multiple instances require external coordination.
type Orchestrator struct {
	store   store.Store
	agents  map[string]agent.Agent
	runRoot string
	mu      sync.Mutex
}

// New uses registry keys as agent names, independently of task.Role.
// The caller supplies a non-nil store and a run root (usually <store-root>/runs).
func New(s store.Store, agents map[string]agent.Agent, runRoot string) *Orchestrator {
	return &Orchestrator{store: s, agents: maps.Clone(agents), runRoot: runRoot}
}

// Run loads a pending task and executes exactly one agent attempt. A failed or
// blocked result is returned without an error unless execution or persistence
// failed. Other starting states return store.ErrStatusConflict.
func (o *Orchestrator) Run(ctx context.Context, taskID, agentName string) (store.Result, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	a, ok := o.agents[agentName]
	if !ok || a == nil {
		return store.Result{}, fmt.Errorf("unknown agent %q", agentName)
	}
	if taskID == "." || !filepath.IsLocal(taskID) || filepath.Base(taskID) != taskID {
		return store.Result{}, fmt.Errorf("invalid task ID %q", taskID)
	}
	task, err := o.store.GetTask(ctx, taskID)
	if err != nil {
		return store.Result{}, fmt.Errorf("load task %q: %w", taskID, err)
	}
	if task.Status != store.StatusPending {
		return store.Result{}, fmt.Errorf("%w: task %q is %q, expected pending", store.ErrStatusConflict, taskID, task.Status)
	}
	runs, err := o.store.ListRuns(ctx, taskID)
	if err != nil {
		return store.Result{}, fmt.Errorf("list runs for %q: %w", taskID, err)
	}
	n := 1
	for _, run := range runs {
		if run.N >= n {
			n = run.N + 1
		}
	}
	runDir, err := filepath.Abs(filepath.Join(o.runRoot, taskID, strconv.Itoa(n)))
	if err != nil {
		return store.Result{}, fmt.Errorf("resolve run directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(runDir), 0o755); err != nil {
		return store.Result{}, fmt.Errorf("create task run directory: %w", err)
	}
	// Refuse to reuse a directory: agents require fresh result files and logs.
	if err := os.Mkdir(runDir, 0o755); err != nil {
		return store.Result{}, fmt.Errorf("create run directory: %w", err)
	}
	if err := o.store.UpdateStatus(ctx, taskID, store.StatusPending, store.StatusRunning); err != nil {
		os.Remove(runDir) // Only remove the empty directory reserved above.
		return store.Result{}, fmt.Errorf("start task %q: %w", taskID, err)
	}
	task.Status = store.StatusRunning

	result, runErr := a.Run(ctx, task, runDir)
	status := store.StatusFailed
	if runErr != nil {
		runErr = fmt.Errorf("run agent %q: %w", agentName, runErr)
	} else {
		switch result.Status {
		case store.ResultStatusDone:
			status = store.StatusDone
		case store.ResultStatusFailed:
			status = store.StatusFailed
		case store.ResultStatusBlocked:
			status = store.StatusBlocked
		default:
			runErr = fmt.Errorf("agent %q returned unknown result status %q", agentName, result.Status)
		}
	}
	if result == (store.Result{}) {
		result = store.Result{ID: taskID, Status: store.ResultStatusFailed, Notes: runErr.Error()}
	} else if result.ID == "" {
		result.ID = taskID
	}
	if result.ID != taskID {
		status = store.StatusFailed
		runErr = errors.Join(runErr, fmt.Errorf("agent %q returned result ID %q, expected %q", agentName, result.ID, taskID))
	}

	// The agent may have returned because ctx was cancelled. Still attempt both
	// writes, retaining all errors and any partial result for the caller.
	persistCtx := context.WithoutCancel(ctx)
	saveErr := o.store.SaveRun(persistCtx, store.Run{TaskID: taskID, N: n, Task: task, Result: result})
	if saveErr != nil {
		status = store.StatusFailed
		saveErr = fmt.Errorf("save run %q/%d: %w", taskID, n, saveErr)
	}
	statusErr := o.store.UpdateStatus(persistCtx, taskID, store.StatusRunning, status)
	if statusErr != nil {
		statusErr = fmt.Errorf("finish task %q: %w", taskID, statusErr)
	}
	return result, errors.Join(runErr, saveErr, statusErr)
}
