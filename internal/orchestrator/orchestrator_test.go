package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hszjj221/troupe/internal/agent"
	"github.com/hszjj221/troupe/internal/store"
)

type fakeAgent func(context.Context, store.Task, string) (store.Result, error)

func (fakeAgent) Name() string { return "fake" }
func (f fakeAgent) Run(ctx context.Context, task store.Task, dir string) (store.Result, error) {
	return f(ctx, task, dir)
}

type recordingStore struct {
	store.Store
	events []string
	update func(context.Context, string, store.Status, store.Status) error
	save   func(context.Context, store.Run) error
}

func (s *recordingStore) UpdateStatus(ctx context.Context, id string, from, to store.Status) error {
	s.events = append(s.events, string(from)+"->"+string(to))
	if s.update != nil {
		return s.update(ctx, id, from, to)
	}
	return s.Store.UpdateStatus(ctx, id, from, to)
}

func (s *recordingStore) SaveRun(ctx context.Context, run store.Run) error {
	s.events = append(s.events, "save")
	if s.save != nil {
		return s.save(ctx, run)
	}
	return s.Store.SaveRun(ctx, run)
}

func newTestStore(t *testing.T) (*recordingStore, string) {
	t.Helper()
	root := t.TempDir()
	s, err := store.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	return &recordingStore{Store: s}, filepath.Join(root, "runs")
}

func saveTask(t *testing.T, s store.Store, status store.Status) store.Task {
	t.Helper()
	task := store.Task{
		ID: "task-1", Goal: "implement a bounded task", Role: "coder",
		Workdir: t.TempDir(), Acceptance: []string{"go test ./..."},
		Context: "Keep existing defaults.", Budget: store.Budget{MaxMinutes: 5}, Status: status,
	}
	if err := s.SaveTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

func assertStatus(t *testing.T, s store.Store, id string, want store.Status) {
	t.Helper()
	task, err := s.GetTask(context.Background(), id)
	if err != nil || task.Status != want {
		t.Fatalf("task status = %q, %v; want %q", task.Status, err, want)
	}
}

func assertRun(t *testing.T, s store.Store, task store.Task, result store.Result, n int) {
	t.Helper()
	runs, err := s.ListRuns(context.Background(), task.ID)
	want := store.Run{TaskID: task.ID, N: n, Task: task, Result: result}
	if err != nil || len(runs) == 0 || !reflect.DeepEqual(runs[len(runs)-1], want) {
		t.Fatalf("ListRuns = %#v, %v; want last run %#v", runs, err, want)
	}
}

func TestRunHappyPath(t *testing.T) {
	s, runRoot := newTestStore(t)
	task := saveTask(t, s, store.StatusPending)
	want := store.Result{ID: task.ID, Status: store.ResultStatusDone, Summary: "implemented", Commit: "abc123", Notes: "verified"}
	running := task
	running.Status = store.StatusRunning
	logPath := filepath.Join(runRoot, task.ID, "1", "logs", "fake.log")
	a := fakeAgent(func(ctx context.Context, got store.Task, dir string) (store.Result, error) {
		s.events = append(s.events, "run")
		if !reflect.DeepEqual(got, running) {
			t.Fatalf("agent task = %#v; want %#v", got, running)
		}
		assertStatus(t, s, task.ID, store.StatusRunning)
		if wantDir := filepath.Join(runRoot, task.ID, "1"); dir != wantDir {
			t.Fatalf("runDir = %q; want %q", dir, wantDir)
		}
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logPath, []byte("agent output"), 0o644); err != nil {
			t.Fatal(err)
		}
		return want, nil
	})
	unused := fakeAgent(func(context.Context, store.Task, string) (store.Result, error) {
		t.Fatal("ran unselected agent")
		return store.Result{}, nil
	})
	o := New(s, map[string]agent.Agent{"chosen": a, "unused": unused}, runRoot)
	got, err := o.Run(context.Background(), task.ID, "chosen")
	if err != nil || got != want {
		t.Fatalf("Run = %#v, %v; want %#v", got, err, want)
	}
	if wantEvents := []string{"pending->running", "run", "save", "running->done"}; !reflect.DeepEqual(s.events, wantEvents) {
		t.Fatalf("events = %v; want %v", s.events, wantEvents)
	}
	assertStatus(t, s, task.ID, store.StatusDone)
	assertRun(t, s, running, want, 1)
	if data, err := os.ReadFile(logPath); err != nil || string(data) != "agent output" {
		t.Fatalf("agent log = %q, %v", data, err)
	}
}

func TestRunUnknownAgent(t *testing.T) {
	s, runRoot := newTestStore(t)
	task := saveTask(t, s, store.StatusPending)
	a := fakeAgent(func(context.Context, store.Task, string) (store.Result, error) {
		t.Fatal("unknown agent caused a run")
		return store.Result{}, nil
	})
	o := New(s, map[string]agent.Agent{"fake": a}, runRoot)
	if _, err := o.Run(context.Background(), task.ID, "missing"); err == nil || !strings.Contains(err.Error(), `unknown agent "missing"`) {
		t.Fatalf("Run = %v; want unknown agent error", err)
	}
	assertStatus(t, s, task.ID, store.StatusPending)
	if len(s.events) != 0 {
		t.Fatalf("unknown agent mutated store: %v", s.events)
	}
}

func TestRunResultStatuses(t *testing.T) {
	for _, tt := range []struct {
		result store.ResultStatus
		status store.Status
		err    bool
	}{
		{store.ResultStatusFailed, store.StatusFailed, false},
		{store.ResultStatusBlocked, store.StatusBlocked, false},
		{"unexpected", store.StatusFailed, true},
	} {
		t.Run(string(tt.result), func(t *testing.T) {
			s, runRoot := newTestStore(t)
			task := saveTask(t, s, store.StatusPending)
			want := store.Result{ID: task.ID, Status: tt.result, Notes: "reason"}
			a := fakeAgent(func(context.Context, store.Task, string) (store.Result, error) { return want, nil })
			got, err := New(s, map[string]agent.Agent{"fake": a}, runRoot).Run(context.Background(), task.ID, "fake")
			if (err != nil) != tt.err || got != want {
				t.Fatalf("Run = %#v, %v; want %#v, error=%v", got, err, want, tt.err)
			}
			assertStatus(t, s, task.ID, tt.status)
			task.Status = store.StatusRunning
			assertRun(t, s, task, want, 1)
		})
	}
}

func TestRunAgentError(t *testing.T) {
	for _, tt := range []struct {
		name   string
		result store.Result
		cancel bool
	}{
		{"partial", store.Result{ID: "task-1", Status: store.ResultStatusDone, Summary: "partial work", Commit: "abc123", Notes: "keep me"}, false},
		{"empty", store.Result{}, false},
		{"cancelled with partial result", store.Result{Status: store.ResultStatusBlocked, Summary: "interrupted"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, runRoot := newTestStore(t)
			task := saveTask(t, s, store.StatusPending)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			agentErr := errors.New("agent stopped")
			if tt.cancel {
				agentErr = context.Canceled
			}
			a := fakeAgent(func(context.Context, store.Task, string) (store.Result, error) {
				if tt.cancel {
					cancel()
				}
				return tt.result, agentErr
			})
			got, err := New(s, map[string]agent.Agent{"fake": a}, runRoot).Run(ctx, task.ID, "fake")
			if !errors.Is(err, agentErr) {
				t.Fatalf("Run = %v; want wrapped %v", err, agentErr)
			}
			want := tt.result
			if want == (store.Result{}) {
				want.Status = store.ResultStatusFailed
				want.Notes = got.Notes
				if !strings.Contains(got.Notes, agentErr.Error()) {
					t.Fatalf("empty result notes = %q; want agent error", got.Notes)
				}
			}
			if want.ID == "" {
				want.ID = task.ID
			}
			if got != want {
				t.Fatalf("Run result = %#v; want %#v", got, want)
			}
			assertStatus(t, s, task.ID, store.StatusFailed)
			task.Status = store.StatusRunning
			assertRun(t, s, task, want, 1)
			if wantEvents := []string{"pending->running", "save", "running->failed"}; !reflect.DeepEqual(s.events, wantEvents) {
				t.Fatalf("events = %v; want %v", s.events, wantEvents)
			}
		})
	}
}

func TestRunMissingOrNonPendingTask(t *testing.T) {
	for _, status := range []store.Status{"", store.StatusRunning, store.StatusDone, store.StatusFailed, store.StatusBlocked} {
		t.Run(string(status), func(t *testing.T) {
			s, runRoot := newTestStore(t)
			wantErr := store.ErrNotFound
			if status != "" {
				saveTask(t, s, status)
				wantErr = store.ErrStatusConflict
			}
			a := fakeAgent(func(context.Context, store.Task, string) (store.Result, error) {
				t.Fatal("ran missing or non-pending task")
				return store.Result{}, nil
			})
			_, err := New(s, map[string]agent.Agent{"fake": a}, runRoot).Run(context.Background(), "task-1", "fake")
			if !errors.Is(err, wantErr) {
				t.Fatalf("Run = %v; want %v", err, wantErr)
			}
			if len(s.events) != 0 {
				t.Fatalf("unexpected store writes: %v", s.events)
			}
			if status != "" {
				assertStatus(t, s, "task-1", status)
			}
		})
	}
}

func TestRunStatusConflict(t *testing.T) {
	for _, phase := range []string{"start", "finish"} {
		t.Run(phase, func(t *testing.T) {
			s, runRoot := newTestStore(t)
			task := saveTask(t, s, store.StatusPending)
			s.update = func(ctx context.Context, id string, from, to store.Status) error {
				if (phase == "start" && from == store.StatusPending) || (phase == "finish" && from == store.StatusRunning) {
					if err := s.Store.UpdateStatus(ctx, id, from, store.StatusBlocked); err != nil {
						t.Fatal(err)
					}
				}
				return s.Store.UpdateStatus(ctx, id, from, to)
			}
			calls := 0
			result := store.Result{ID: task.ID, Status: store.ResultStatusDone}
			a := fakeAgent(func(context.Context, store.Task, string) (store.Result, error) {
				calls++
				return result, nil
			})
			_, err := New(s, map[string]agent.Agent{"fake": a}, runRoot).Run(context.Background(), task.ID, "fake")
			if !errors.Is(err, store.ErrStatusConflict) {
				t.Fatalf("Run = %v; want status conflict", err)
			}
			assertStatus(t, s, task.ID, store.StatusBlocked)
			if phase == "start" {
				if calls != 0 {
					t.Fatalf("agent calls = %d; want 0", calls)
				}
				if runs, err := s.ListRuns(context.Background(), task.ID); err != nil || len(runs) != 0 {
					t.Fatalf("ListRuns after conflict = %#v, %v; want no runs", runs, err)
				}
			} else {
				if calls != 1 {
					t.Fatalf("agent calls = %d; want 1", calls)
				}
				task.Status = store.StatusRunning
				assertRun(t, s, task, result, 1)
			}
		})
	}
}

func TestRunSaveErrorStillFinishes(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"agent succeeded", nil},
		{"agent failed", errors.New("agent stopped")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, runRoot := newTestStore(t)
			task := saveTask(t, s, store.StatusPending)
			saveErr := errors.New("storage unavailable")
			s.save = func(context.Context, store.Run) error { return saveErr }
			result := store.Result{ID: task.ID, Status: store.ResultStatusDone, Summary: "work completed"}
			a := fakeAgent(func(context.Context, store.Task, string) (store.Result, error) { return result, tt.err })
			got, err := New(s, map[string]agent.Agent{"fake": a}, runRoot).Run(context.Background(), task.ID, "fake")
			if !errors.Is(err, saveErr) || (tt.err != nil && !errors.Is(err, tt.err)) || got != result {
				t.Fatalf("Run = %#v, %v; want result and persistence/execution errors", got, err)
			}
			assertStatus(t, s, task.ID, store.StatusFailed)
			if wantEvents := []string{"pending->running", "save", "running->failed"}; !reflect.DeepEqual(s.events, wantEvents) {
				t.Fatalf("events = %v; want %v", s.events, wantEvents)
			}
		})
	}
}

func TestRunMismatchedResultID(t *testing.T) {
	s, runRoot := newTestStore(t)
	task := saveTask(t, s, store.StatusPending)
	result := store.Result{ID: "other-task", Status: store.ResultStatusDone}
	a := fakeAgent(func(context.Context, store.Task, string) (store.Result, error) { return result, nil })
	got, err := New(s, map[string]agent.Agent{"fake": a}, runRoot).Run(context.Background(), task.ID, "fake")
	if err == nil || !strings.Contains(err.Error(), `returned result ID "other-task"`) || got != result {
		t.Fatalf("Run = %#v, %v; want preserved result and ID mismatch error", got, err)
	}
	assertStatus(t, s, task.ID, store.StatusFailed)
	if wantEvents := []string{"pending->running", "save", "running->failed"}; !reflect.DeepEqual(s.events, wantEvents) {
		t.Fatalf("events = %v; want %v", s.events, wantEvents)
	}
}

func TestRunNextRound(t *testing.T) {
	s, runRoot := newTestStore(t)
	task := saveTask(t, s, store.StatusPending)
	previous := store.Run{TaskID: task.ID, N: 3, Task: task, Result: store.Result{ID: task.ID, Status: store.ResultStatusBlocked}}
	if err := s.Store.SaveRun(context.Background(), previous); err != nil {
		t.Fatal(err)
	}
	result := store.Result{ID: task.ID, Status: store.ResultStatusDone}
	a := fakeAgent(func(_ context.Context, _ store.Task, dir string) (store.Result, error) {
		if want := filepath.Join(runRoot, task.ID, "4"); dir != want {
			t.Fatalf("runDir = %q; want %q", dir, want)
		}
		return result, nil
	})
	if _, err := New(s, map[string]agent.Agent{"fake": a}, runRoot).Run(context.Background(), task.ID, "fake"); err != nil {
		t.Fatal(err)
	}
	task.Status = store.StatusRunning
	assertRun(t, s, task, result, 4)
	runs, err := s.ListRuns(context.Background(), task.ID)
	if err != nil || len(runs) != 2 || !reflect.DeepEqual(runs[0], previous) {
		t.Fatalf("previous run changed: %#v, %v", runs, err)
	}
}
