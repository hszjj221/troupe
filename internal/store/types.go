package store

import "context"

type Store interface {
	SaveTask(ctx context.Context, t Task) error
	GetTask(ctx context.Context, id string) (Task, error)
	ListTasks(ctx context.Context, filter TaskFilter) ([]Task, error)
	// UpdateStatus changes the status only when the current status equals from.
	UpdateStatus(ctx context.Context, id string, from, to Status) error
	SaveRun(ctx context.Context, r Run) error
	ListRuns(ctx context.Context, taskID string) ([]Run, error)
}

// Status is a task's persisted state; transition rules are left to the caller.
type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	StatusBlocked Status = "blocked"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

type Task struct {
	ID         string   `json:"id" yaml:"id"`
	Goal       string   `json:"goal" yaml:"goal"`
	Role       string   `json:"role" yaml:"role"`
	Workdir    string   `json:"workdir" yaml:"workdir"`
	Acceptance []string `json:"acceptance" yaml:"acceptance"`
	Context    string   `json:"context" yaml:"context"`
	Budget     Budget   `json:"budget" yaml:"budget"`
	Status     Status   `json:"status" yaml:"status,omitempty"`
}

type Budget struct {
	MaxMinutes int `json:"max_minutes" yaml:"max_minutes"`
}

type ResultStatus string

const (
	ResultStatusDone    ResultStatus = "done"
	ResultStatusFailed  ResultStatus = "failed"
	ResultStatusBlocked ResultStatus = "blocked"
)

type Result struct {
	ID      string       `json:"id"`
	Status  ResultStatus `json:"status"`
	Summary string       `json:"summary"`
	Commit  string       `json:"commit"`
	Notes   string       `json:"notes"`
}

type Run struct {
	TaskID string
	N      int
	Task   Task
	Result Result
}

type TaskFilter struct {
	Status Status // Empty matches all tasks.
}
