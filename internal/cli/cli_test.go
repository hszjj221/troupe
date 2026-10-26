package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hszjj221/troupe/internal/agent"
	"github.com/hszjj221/troupe/internal/config"
	"github.com/hszjj221/troupe/internal/store"
)

type fakeAgent struct {
	name string
	run  func(context.Context, store.Task, string) (store.Result, error)
}

func (a fakeAgent) Name() string { return a.name }
func (a fakeAgent) Run(ctx context.Context, task store.Task, dir string) (store.Result, error) {
	return a.run(ctx, task, dir)
}

func TestRun(t *testing.T) {
	t.Setenv("TROUPE_WEBHOOK_URL", "")
	for _, tt := range []struct {
		name         string
		agentName    string
		flags        []string
		resultStatus store.ResultStatus
		taskStatus   store.Status
		code         int
		wantErr      string
		agentErr     error
		breakArchive bool
	}{
		{name: "done with defaults", agentName: "codex", resultStatus: store.ResultStatusDone, taskStatus: store.StatusDone},
		{
			name: "done with options", agentName: "claude", resultStatus: store.ResultStatusDone, taskStatus: store.StatusDone,
			flags: []string{"--id", "chosen-id", "--role", "coder", "--workdir", "project", "--context", "Keep defaults.",
				"--acceptance", "go test ./...", "--acceptance", "printf 'a,b'", "--budget-minutes", "2"},
		},
		{name: "blocked", agentName: "codex", resultStatus: store.ResultStatusBlocked, taskStatus: store.StatusBlocked, code: 1},
		{name: "failed", agentName: "claude", resultStatus: store.ResultStatusFailed, taskStatus: store.StatusFailed, code: 1},
		{
			name: "agent error with partial result", agentName: "codex", resultStatus: store.ResultStatusDone,
			taskStatus: store.StatusFailed, code: 1, agentErr: errors.New("agent stopped"), wantErr: "agent stopped",
		},
		{
			name: "run persistence error", agentName: "codex", resultStatus: store.ResultStatusDone,
			taskStatus: store.StatusFailed, code: 1, breakArchive: true, wantErr: "save run",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TROUPE_HOME", "")
			t.Chdir(t.TempDir())
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(cwd, "state")
			wantTask := store.Task{
				Goal: "Implement a bounded task", Workdir: cwd,
				Budget: store.Budget{MaxMinutes: 30}, Status: store.StatusRunning,
			}
			if len(tt.flags) != 0 {
				wantTask.ID = "chosen-id"
				wantTask.Role = "coder"
				wantTask.Workdir = filepath.Join(cwd, "project")
				wantTask.Context = "Keep defaults."
				wantTask.Acceptance = []string{"go test ./...", "printf 'a,b'"}
				wantTask.Budget.MaxMinutes = 2
			}
			calls := 0
			var wantResult store.Result
			a := fakeAgent{name: tt.agentName, run: func(ctx context.Context, task store.Task, dir string) (store.Result, error) {
				calls++
				if wantTask.ID == "" {
					if !strings.HasPrefix(task.ID, "t-") || task.ID == "t-" {
						t.Fatalf("generated ID = %q; want t- prefixed ID", task.ID)
					}
					wantTask.ID = task.ID
				}
				if !reflect.DeepEqual(task, wantTask) {
					t.Fatalf("agent task = %#v; want %#v", task, wantTask)
				}
				fs := openStore(t, root)
				if saved, err := fs.GetTask(ctx, task.ID); err != nil || !reflect.DeepEqual(saved, task) {
					t.Fatalf("task was not saved before Run: %#v, %v", saved, err)
				}
				if want := filepath.Join(root, "runs", task.ID, "1"); dir != want {
					t.Fatalf("run directory = %q; want %q", dir, want)
				}
				deadline, ok := ctx.Deadline()
				budget := time.Duration(task.Budget.MaxMinutes) * time.Minute
				if remaining := time.Until(deadline); !ok || remaining > budget || remaining < budget-5*time.Second {
					t.Fatalf("agent deadline = %v, %v; want budget %v", deadline, ok, budget)
				}
				if tt.breakArchive {
					if err := os.WriteFile(filepath.Join(dir, "logs"), []byte("obstruct archive"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				wantResult = store.Result{ID: task.ID, Status: tt.resultStatus, Summary: "Agent summary", Commit: "abc123", Notes: "Agent notes"}
				return wantResult, tt.agentErr
			}}
			otherName := "claude"
			if tt.agentName == otherName {
				otherName = "codex"
			}
			other := fakeAgent{name: otherName, run: func(context.Context, store.Task, string) (store.Result, error) {
				t.Fatal("ran unselected agent")
				return store.Result{}, nil
			}}
			args := []string{"run", "--agent", tt.agentName, "--goal", wantTask.Goal, "--store", root}
			args = append(args, tt.flags...)
			var stdout, stderr bytes.Buffer
			code := execute(context.Background(), args, &stdout, &stderr, map[string]agent.Agent{a.Name(): a, other.Name(): other}, nil)
			if code != tt.code || calls != 1 {
				t.Fatalf("exit = %d, agent calls = %d; want %d, 1; stderr: %s", code, calls, tt.code, &stderr)
			}
			if tt.wantErr == "" && stderr.Len() != 0 || tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q; want error %q", &stderr, tt.wantErr)
			}
			wantOutput := "ID: " + wantResult.ID + "\nStatus: " + string(tt.resultStatus) + "\nSummary: Agent summary\nCommit: abc123\nNotes: Agent notes\n"
			if stdout.String() != wantOutput {
				t.Fatalf("stdout = %q; want %q", &stdout, wantOutput)
			}
			fs := openStore(t, root)
			wantTask.Status = tt.taskStatus
			if got, err := fs.GetTask(context.Background(), wantTask.ID); err != nil || !reflect.DeepEqual(got, wantTask) {
				t.Fatalf("persisted task = %#v, %v; want %#v", got, err, wantTask)
			}
			if !tt.breakArchive {
				wantTask.Status = store.StatusRunning
				// YAML round-tripping normalizes a nil acceptance list to empty.
				if wantTask.Acceptance == nil {
					wantTask.Acceptance = []string{}
				}
				wantRuns := []store.Run{{TaskID: wantTask.ID, N: 1, Task: wantTask, Result: wantResult}}
				if got, err := fs.ListRuns(context.Background(), wantTask.ID); err != nil || !reflect.DeepEqual(got, wantRuns) {
					t.Fatalf("archived runs = %#v, %v; want %#v", got, err, wantRuns)
				}
			}
		})
	}
}

func TestResume(t *testing.T) {
	t.Setenv("TROUPE_WEBHOOK_URL", "")
	for _, tt := range []struct {
		name        string
		agentName   string
		args        []string
		context     string
		wantContext string
		status      store.ResultStatus
		taskStatus  store.Status
		agentErr    error
		code        int
	}{
		{
			name: "ID before flags with reply", agentName: "codex", args: []string{"task-1", "--agent", "codex", "--note", "Use approach A."},
			context: "Original context", wantContext: "Original context\n\nUser reply: Use approach A.",
			status: store.ResultStatusDone, taskStatus: store.StatusDone,
		},
		{
			name: "flags before ID without reply", agentName: "claude", args: []string{"--agent", "claude", "task-1"},
			context: "Original context", wantContext: "Original context", status: store.ResultStatusDone, taskStatus: store.StatusDone,
		},
		{
			name: "flags on both sides with empty context", agentName: "codex", args: []string{"--agent", "codex", "task-1", "--note", "Use approach A."},
			wantContext: "User reply: Use approach A.", status: store.ResultStatusDone, taskStatus: store.StatusDone,
		},
		{
			name: "failed", agentName: "codex", args: []string{"task-1", "--agent", "codex"},
			status: store.ResultStatusFailed, taskStatus: store.StatusFailed, code: 1,
		},
		{
			name: "error with partial result", agentName: "codex", args: []string{"task-1", "--agent", "codex"},
			status: store.ResultStatusDone, taskStatus: store.StatusFailed, agentErr: errors.New("agent stopped"), code: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			fs := openStore(t, root)
			task := store.Task{
				ID: "task-1", Goal: "Finish existing task", Role: "coder", Workdir: t.TempDir(),
				Acceptance: []string{"go test ./..."}, Context: tt.context,
				Budget: store.Budget{MaxMinutes: 2}, Status: store.StatusBlocked,
			}
			if err := fs.SaveTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			previous := store.Run{TaskID: task.ID, N: 1, Task: task, Result: store.Result{ID: task.ID, Status: store.ResultStatusBlocked}}
			previous.Task.Status = store.StatusRunning
			if err := fs.SaveRun(ctx, previous); err != nil {
				t.Fatal(err)
			}
			wantTask := task
			wantTask.Context = tt.wantContext
			wantTask.Status = store.StatusRunning
			result := store.Result{ID: task.ID, Status: tt.status, Summary: "Resumed summary", Commit: "abc123", Notes: "Resumed notes"}
			calls := 0
			a := fakeAgent{name: tt.agentName, run: func(ctx context.Context, got store.Task, dir string) (store.Result, error) {
				calls++
				if !reflect.DeepEqual(got, wantTask) {
					t.Fatalf("agent task = %#v; want %#v", got, wantTask)
				}
				if saved, err := fs.GetTask(ctx, task.ID); err != nil || !reflect.DeepEqual(saved, wantTask) {
					t.Fatalf("persisted task at agent call = %#v, %v; want %#v", saved, err, wantTask)
				}
				if want := filepath.Join(root, "runs", task.ID, "2"); dir != want {
					t.Fatalf("run directory = %q; want next round %q", dir, want)
				}
				deadline, ok := ctx.Deadline()
				budget := time.Duration(task.Budget.MaxMinutes) * time.Minute
				if remaining := time.Until(deadline); !ok || remaining > budget || remaining < budget-5*time.Second {
					t.Fatalf("agent deadline = %v, %v; want stored budget %v", deadline, ok, budget)
				}
				return result, tt.agentErr
			}}
			other := fakeAgent{run: func(context.Context, store.Task, string) (store.Result, error) {
				t.Fatal("ran unselected agent")
				return store.Result{}, nil
			}}
			agents := map[string]agent.Agent{"codex": other, "claude": other}
			agents[a.Name()] = a
			args := append([]string{"resume"}, tt.args...)
			args = append(args, "--store", root)
			var stdout, stderr bytes.Buffer
			code := execute(ctx, args, &stdout, &stderr, agents, nil)
			if code != tt.code || calls != 1 {
				t.Fatalf("exit = %d, agent calls = %d; want %d, 1; stderr: %s", code, calls, tt.code, &stderr)
			}
			if tt.agentErr == nil && stderr.Len() != 0 || tt.agentErr != nil && !strings.Contains(stderr.String(), tt.agentErr.Error()) {
				t.Fatalf("stderr = %q; want error %v", &stderr, tt.agentErr)
			}
			wantOutput := "ID: task-1\nStatus: " + string(tt.status) + "\nSummary: Resumed summary\nCommit: abc123\nNotes: Resumed notes\n"
			if stdout.String() != wantOutput {
				t.Fatalf("stdout = %q; want %q", &stdout, wantOutput)
			}
			wantTask.Status = tt.taskStatus
			if saved, err := fs.GetTask(ctx, task.ID); err != nil || !reflect.DeepEqual(saved, wantTask) {
				t.Fatalf("persisted task = %#v, %v; want %#v", saved, err, wantTask)
			}
			wantTask.Status = store.StatusRunning
			wantRuns := []store.Run{previous, {TaskID: task.ID, N: 2, Task: wantTask, Result: result}}
			if runs, err := fs.ListRuns(ctx, task.ID); err != nil || !reflect.DeepEqual(runs, wantRuns) {
				t.Fatalf("archived runs = %#v, %v; want %#v", runs, err, wantRuns)
			}
		})
	}
}

func TestResumeRejectsUnresumableTasks(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status store.Status
		budget int
		err    string
	}{
		{"pending", store.StatusPending, 2, `current status is "pending"; expected blocked`},
		{"running", store.StatusRunning, 2, `current status is "running"; expected blocked`},
		{"done", store.StatusDone, 2, `current status is "done"; expected blocked`},
		{"failed", store.StatusFailed, 2, `current status is "failed"; expected blocked`},
		{"unknown", "unknown", 2, `current status is "unknown"; expected blocked`},
		{"empty status", "", 2, `current status is ""; expected blocked`},
		{"zero budget", store.StatusBlocked, 0, "budget max_minutes must be between"},
		{"negative budget", store.StatusBlocked, -1, "budget max_minutes must be between"},
		{"overflowing budget", store.StatusBlocked, 153722868, "budget max_minutes must be between"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			fs := openStore(t, root)
			task := store.Task{ID: "task-1", Context: "Original context", Status: tt.status, Budget: store.Budget{MaxMinutes: tt.budget}}
			if err := fs.SaveTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			a := fakeAgent{name: "codex", run: func(context.Context, store.Task, string) (store.Result, error) {
				t.Fatal("agent ran for an unresumable task")
				return store.Result{}, nil
			}}
			args := []string{"resume", task.ID, "--agent", "codex", "--store", root, "--note", "Should not be saved"}
			var stdout, stderr bytes.Buffer
			if code := execute(ctx, args, &stdout, &stderr, map[string]agent.Agent{a.Name(): a}, nil); code != 1 || !strings.Contains(stderr.String(), tt.err) || stdout.Len() != 0 {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q; want 1 and %q", code, &stdout, &stderr, tt.err)
			}
			if saved, err := fs.GetTask(ctx, task.ID); err != nil || !reflect.DeepEqual(saved, task) {
				t.Fatalf("rejected task changed: %#v, %v; want %#v", saved, err, task)
			}
		})
	}
}

func TestStatus(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fs := openStore(t, root)
	task := store.Task{ID: "task-1", Status: store.StatusBlocked, Goal: "Finish existing task", Role: "coder", Workdir: "/workspace/project"}
	if err := fs.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"status", task.ID, "--store", root},
		{"status", "--store", root, task.ID},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := execute(ctx, args, &stdout, &stderr, nil, nil); code != 0 || stderr.Len() != 0 {
				t.Fatalf("exit = %d, stderr = %q; want 0 and no error", code, &stderr)
			}
			want := "ID: task-1\nStatus: blocked\nGoal: Finish existing task\nRole: coder\nWorkdir: /workspace/project\n"
			if stdout.String() != want {
				t.Fatalf("stdout = %q; want %q", &stdout, want)
			}
		})
	}
	t.Run("missing task", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		args := []string{"status", "missing", "--store", root}
		if code := execute(ctx, args, &stdout, &stderr, nil, nil); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), store.ErrNotFound.Error()) {
			t.Fatalf("exit = %d, stdout = %q, stderr = %q; want 1 and task not found", code, &stdout, &stderr)
		}
	})
}

func TestList(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fs := openStore(t, root)
	for _, task := range []store.Task{
		{ID: "e", Status: store.StatusFailed, Goal: "Failed task"},
		{ID: "c", Status: store.StatusBlocked, Goal: "Need\n a\tdecision"},
		{ID: "a", Status: store.StatusPending, Goal: "Pending task"},
		{ID: "d", Status: store.StatusDone, Goal: strings.Repeat("界", 81)},
		{ID: "b", Status: store.StatusRunning, Goal: strings.Repeat("x", 80)},
	} {
		if err := fs.SaveTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name  string
		flags []string
		root  string
		want  string
	}{
		{"all", nil, root, "a\tpending\tPending task\nb\trunning\t" + strings.Repeat("x", 80) + "\nc\tblocked\tNeed a decision\nd\tdone\t" + strings.Repeat("界", 79) + "…\ne\tfailed\tFailed task\n"},
		{"pending", []string{"--status", "pending"}, root, "a\tpending\tPending task\n"},
		{"running", []string{"--status", "running"}, root, "b\trunning\t" + strings.Repeat("x", 80) + "\n"},
		{"blocked", []string{"--status", "blocked"}, root, "c\tblocked\tNeed a decision\n"},
		{"done", []string{"--status", "done"}, root, "d\tdone\t" + strings.Repeat("界", 79) + "…\n"},
		{"failed", []string{"--status", "failed"}, root, "e\tfailed\tFailed task\n"},
		{"empty", nil, t.TempDir(), ""},
		{"empty filtered", []string{"--status", "blocked"}, t.TempDir(), ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"list", "--store", tt.root}, tt.flags...)
			var stdout, stderr bytes.Buffer
			if code := execute(ctx, args, &stdout, &stderr, nil, nil); code != 0 || stderr.Len() != 0 || stdout.String() != tt.want {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q; want 0, %q and no error", code, &stdout, &stderr, tt.want)
			}
		})
	}
}

func TestStatusAndListAreReadOnly(t *testing.T) {
	t.Setenv("TROUPE_WEBHOOK_URL", "https://notify.example/blocked")
	t.Setenv("TROUPE_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	ctx := context.Background()
	root := t.TempDir()
	fs := openStore(t, root)
	task := store.Task{ID: "task-1", Goal: "Need a decision", Status: store.StatusBlocked, Context: "Keep context", Budget: store.Budget{MaxMinutes: 2}}
	if err := fs.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	a := fakeAgent{name: "codex", run: func(context.Context, store.Task, string) (store.Result, error) {
		t.Fatal("read-only command ran an agent")
		return store.Result{}, nil
	}}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("read-only command sent a webhook")
		return nil, errors.New("unexpected webhook")
	})}
	for _, args := range [][]string{{"status", task.ID, "--store", root}, {"list", "--store", root}} {
		var stdout, stderr bytes.Buffer
		if code := execute(ctx, args, &stdout, &stderr, map[string]agent.Agent{a.Name(): a}, client); code != 0 {
			t.Fatalf("%s exit = %d, stderr = %q; want 0", args[0], code, &stderr)
		}
		if got, err := fs.GetTask(ctx, task.ID); err != nil || !reflect.DeepEqual(got, task) {
			t.Fatalf("%s changed task: %#v, %v; want %#v", args[0], got, err, task)
		}
		if runs, err := fs.ListRuns(ctx, task.ID); err != nil || len(runs) != 0 {
			t.Fatalf("%s created runs: %#v, %v", args[0], runs, err)
		}
	}
}

func TestStatusAndListErrors(t *testing.T) {
	for _, command := range []string{"status", "list"} {
		for _, failure := range []string{"open store", "corrupt task", "output"} {
			t.Run(command+"/"+failure, func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "store")
				wantErr := "open store"
				if failure == "open store" {
					if err := os.WriteFile(root, []byte("file"), 0o644); err != nil {
						t.Fatal(err)
					}
				} else {
					fs := openStore(t, root)
					if err := fs.SaveTask(context.Background(), store.Task{ID: "task-1", Status: store.StatusDone}); err != nil {
						t.Fatal(err)
					}
					wantErr = "output unavailable"
					if failure == "corrupt task" {
						wantErr = "decode task"
						if err := os.WriteFile(filepath.Join(root, "tasks", "task-1.json"), []byte("invalid JSON"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
				}
				args := []string{command, "--store", root}
				if command == "status" {
					args = append(args, "task-1")
				}
				var stdout, stderr bytes.Buffer
				var output io.Writer = &stdout
				if failure == "output" {
					output = failingWriter{}
				}
				if code := execute(context.Background(), args, output, &stderr, nil, nil); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), wantErr) {
					t.Fatalf("exit = %d, stdout = %q, stderr = %q; want 1 and %q", code, &stdout, &stderr, wantErr)
				}
			})
		}
	}
}

func TestValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("TROUPE_CONFIG", "")
	for _, tt := range []struct {
		name string
		args []string
		err  string
	}{
		{"no subcommand", nil, "expected run, resume, status, or list subcommand"},
		{"unknown subcommand", []string{"unknown"}, `unknown subcommand "unknown"`},
		{"missing goal and agent", []string{"run"}, "--goal is required"},
		{"missing goal", []string{"run", "--agent", "codex"}, "--goal is required"},
		{"blank goal", []string{"run", "--goal", "  ", "--agent", "codex"}, "--goal is required"},
		{"missing agent", []string{"run", "--goal", "task"}, "need --agent or config"},
		{"unknown agent", []string{"run", "--goal", "task", "--agent", "missing"}, `unknown agent "missing"`},
		{"positional goal", []string{"run", "--agent", "codex", "task"}, "use --goal"},
		{"unknown flag", []string{"run", "--unknown"}, "flag provided but not defined"},
		{"missing flag value", []string{"run", "--goal"}, "flag needs an argument"},
		{"zero budget", []string{"run", "--goal", "task", "--agent", "codex", "--budget-minutes", "0"}, "--budget-minutes must be between"},
		{"negative budget", []string{"run", "--goal", "task", "--agent", "codex", "--budget-minutes", "-1"}, "--budget-minutes must be between"},
		{"overflowing budget", []string{"run", "--goal", "task", "--agent", "codex", "--budget-minutes", "153722868"}, "--budget-minutes must be between"},
		{"empty acceptance", []string{"run", "--goal", "task", "--agent", "codex", "--acceptance", " "}, "acceptance command must not be empty"},
		{"empty store", []string{"run", "--goal", "task", "--agent", "codex", "--store", ""}, "--store must not be empty"},
		{"resume missing ID and agent", []string{"resume"}, "task ID is required"},
		{"resume missing ID", []string{"resume", "--agent", "codex"}, "task ID is required"},
		{"resume extra ID", []string{"resume", "--agent", "codex", "task-1", "task-2"}, "expected exactly one task ID"},
		{"resume extra ID after flags", []string{"resume", "task-1", "--agent", "codex", "task-2"}, "expected exactly one task ID"},
		{"resume unknown flag", []string{"resume", "task-1", "--unknown"}, "flag provided but not defined"},
		{"resume missing flag value", []string{"resume", "task-1", "--agent", "codex", "--note"}, "flag needs an argument"},
		{"resume empty store", []string{"resume", "task-1", "--agent", "codex", "--store="}, "--store must not be empty"},
		{"status missing ID", []string{"status"}, "task ID is required"},
		{"status extra ID", []string{"status", "task-1", "task-2"}, "expected exactly one task ID"},
		{"status extra ID after flags", []string{"status", "task-1", "--store", "unused", "task-2"}, "expected exactly one task ID"},
		{"status unknown flag", []string{"status", "task-1", "--unknown"}, "flag provided but not defined"},
		{"status missing flag value", []string{"status", "task-1", "--store"}, "flag needs an argument"},
		{"status empty store", []string{"status", "task-1", "--store="}, "--store must not be empty"},
		{"list extra ID", []string{"list", "task-1"}, "unexpected positional arguments"},
		{"list unknown flag", []string{"list", "--unknown"}, "flag provided but not defined"},
		{"list invalid status", []string{"list", "--status", "unknown"}, "choose pending, running, blocked, done, failed"},
		{"list empty status", []string{"list", "--status="}, "choose pending, running, blocked, done, failed"},
		{"list missing status value", []string{"list", "--status"}, "flag needs an argument"},
		{"list empty store", []string{"list", "--store="}, "--store must not be empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "unused-store")
			t.Setenv("TROUPE_HOME", root)
			a := fakeAgent{name: "codex", run: func(context.Context, store.Task, string) (store.Result, error) {
				t.Fatal("agent ran for invalid usage")
				return store.Result{}, nil
			}}
			var stdout, stderr bytes.Buffer
			if code := execute(context.Background(), tt.args, &stdout, &stderr, map[string]agent.Agent{a.Name(): a}, nil); code != 1 {
				t.Fatalf("exit = %d; want 1", code)
			}
			if !strings.Contains(stderr.String(), tt.err) || !strings.Contains(stderr.String(), "Usage:") || stdout.Len() != 0 {
				t.Fatalf("stdout = %q, stderr = %q; want usage and %q on stderr", &stdout, &stderr, tt.err)
			}
			if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid usage created store: %v", err)
			}
		})
	}
}

func TestStorePrecedence(t *testing.T) {
	for _, command := range []string{"run", "resume", "status", "list"} {
		t.Run(command, func(t *testing.T) { testStorePrecedence(t, command) })
	}
}

func testStorePrecedence(t *testing.T, command string) {
	for _, tt := range []struct {
		name string
		env  string
		flag string
		root string
	}{
		{"default", "", "", ".troupe"},
		{"environment", "env-store", "", "env-store"},
		{"flag", "env-store", "flag-store", "flag-store"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("TROUPE_HOME", tt.env)
			root, err := filepath.Abs(tt.root)
			if err != nil {
				t.Fatal(err)
			}
			a := fakeAgent{name: "codex", run: func(_ context.Context, task store.Task, dir string) (store.Result, error) {
				if command == "status" || command == "list" {
					t.Fatal("read-only command ran an agent")
				}
				if want := filepath.Join(root, "runs", "task-1", "1"); dir != want {
					t.Fatalf("run directory = %q; want %q", dir, want)
				}
				return store.Result{ID: task.ID, Status: store.ResultStatusDone}, nil
			}}
			args := []string{"run", "--goal", "task", "--agent", "codex", "--id", "task-1"}
			if command != "run" {
				task := store.Task{ID: "task-1", Status: store.StatusBlocked, Budget: store.Budget{MaxMinutes: 2}}
				if err := openStore(t, root).SaveTask(context.Background(), task); err != nil {
					t.Fatal(err)
				}
				args = []string{"resume", "--agent", "codex", "task-1"}
				if command == "status" {
					args = []string{"status", "task-1"}
				} else if command == "list" {
					args = []string{"list"}
				}
			}
			if tt.flag != "" {
				args = append(args, "--store", tt.flag)
			}
			var stdout, stderr bytes.Buffer
			if code := execute(context.Background(), args, &stdout, &stderr, map[string]agent.Agent{a.Name(): a}, nil); code != 0 {
				t.Fatalf("exit = %d; stderr: %s", code, &stderr)
			}
			wantStatus := store.StatusDone
			if command == "status" || command == "list" {
				wantStatus = store.StatusBlocked
				if !strings.Contains(stdout.String(), "task-1") {
					t.Fatalf("selected store task missing from stdout: %q", &stdout)
				}
			}
			if task, err := openStore(t, root).GetTask(context.Background(), "task-1"); err != nil || task.Status != wantStatus {
				t.Fatalf("task in selected store = %#v, %v", task, err)
			}
			if tt.flag != "" {
				if _, err := os.Stat(tt.env); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("flag did not override environment store: %v", err)
				}
			}
		})
	}
}

func TestStoreErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		id   string
		err  string
	}{
		{"invalid ID", "../escape", "invalid task ID"},
		{"store root is a file", "task-1", "open store"},
		{"existing task", "task-1", "already exists"},
		{"run directory is a file", "task-1", "run task"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "store")
			original := store.Task{ID: tt.id, Goal: "Existing task", Status: store.StatusDone}
			if tt.name == "store root is a file" {
				if err := os.WriteFile(root, []byte("file"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				fs := openStore(t, root)
				if tt.name == "existing task" {
					if err := fs.SaveTask(context.Background(), original); err != nil {
						t.Fatal(err)
					}
				}
				if tt.name == "run directory is a file" {
					if err := os.WriteFile(filepath.Join(root, "runs", tt.id), []byte("file"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			a := fakeAgent{name: "codex", run: func(context.Context, store.Task, string) (store.Result, error) {
				t.Fatal("agent ran despite store error")
				return store.Result{}, nil
			}}
			args := []string{"run", "--goal", "task", "--agent", "codex", "--store", root, "--id", tt.id}
			var stdout, stderr bytes.Buffer
			if code := execute(context.Background(), args, &stdout, &stderr, map[string]agent.Agent{a.Name(): a}, nil); code != 1 || !strings.Contains(stderr.String(), tt.err) {
				t.Fatalf("exit = %d, stderr = %q; want 1 and %q", code, &stderr, tt.err)
			}
			if tt.name == "existing task" {
				if got, err := openStore(t, root).GetTask(context.Background(), tt.id); err != nil || !reflect.DeepEqual(got, original) {
					t.Fatalf("existing task changed: %#v, %v", got, err)
				}
			}
			if tt.name == "run directory is a file" {
				if got, err := openStore(t, root).GetTask(context.Background(), tt.id); err != nil || got.Status != store.StatusPending {
					t.Fatalf("task was not saved pending: %#v, %v", got, err)
				}
			}
		})
	}
}

func TestHelpAndDefaultAgents(t *testing.T) {
	t.Setenv("TROUPE_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"run", "--help"}, {"resume", "--help"}, {"resume", "task-1", "--help"}, {"status", "--help"}, {"status", "task-1", "--help"}, {"list", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Execute(context.Background(), args, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String()+stderr.String(), "Usage:") {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q; want help and exit 0", code, &stdout, &stderr)
			}
			if len(args) == 1 {
				for _, command := range []string{"run", "resume", "status", "list"} {
					if !strings.Contains(stdout.String(), "troupe "+command) {
						t.Fatalf("top-level help must mention %s: %s", command, &stdout)
					}
				}
			} else if !strings.Contains(stderr.String(), "Usage: troupe "+args[0]) {
				t.Fatalf("command help must mention %s: %s", args[0], &stderr)
			}
			if len(args) == 1 || args[0] == "run" || args[0] == "resume" {
				help := stdout.String() + stderr.String()
				if !strings.Contains(help, "--config") || !strings.Contains(help, "role binding") || !strings.Contains(help, "default_agent") {
					t.Fatalf("help must describe config agent selection: %s", help)
				}
			}
		})
	}
	registered := agentsFromConfig(config.Config{})
	if len(registered) != 2 {
		t.Fatalf("registered agents = %v; want codex and claude", registered)
	}
	if a, ok := registered["codex"].(*agent.Codex); !ok || a.Name() != "codex" {
		t.Fatalf("codex registration = %#v", registered["codex"])
	}
	if a, ok := registered["claude"].(*agent.Claude); !ok || a.Name() != "claude" {
		t.Fatalf("claude registration = %#v", registered["claude"])
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }

func TestOutputError(t *testing.T) {
	root := t.TempDir()
	a := fakeAgent{name: "codex", run: func(_ context.Context, task store.Task, _ string) (store.Result, error) {
		return store.Result{ID: task.ID, Status: store.ResultStatusDone}, nil
	}}
	var stderr bytes.Buffer
	args := []string{"run", "--goal", "task", "--agent", "codex", "--store", root}
	if code := execute(context.Background(), args, failingWriter{}, &stderr, map[string]agent.Agent{a.Name(): a}, nil); code != 1 || !strings.Contains(stderr.String(), "write result: output unavailable") {
		t.Fatalf("exit = %d, stderr = %q; want output error", code, &stderr)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBlockedWebhook(t *testing.T) {
	const envURL = "https://notify.example/env?source=troupe"
	const flagURL = "https://notify.example/flag?source=troupe"
	for _, tt := range []struct {
		name         string
		status       store.ResultStatus
		env          string
		flags        []string
		wantURL      string
		code         int
		responseCode int
		clientErr    error
		agentErr     error
		outputError  bool
		wantErr      string
	}{
		{name: "blocked flag wins", status: store.ResultStatusBlocked, env: envURL, flags: []string{"--webhook-url", flagURL}, wantURL: flagURL, code: 1},
		{name: "blocked environment", status: store.ResultStatusBlocked, env: envURL, wantURL: envURL, code: 1},
		{name: "blocked no URL", status: store.ResultStatusBlocked, code: 1},
		{name: "empty flag disables environment", status: store.ResultStatusBlocked, env: envURL, flags: []string{"--webhook-url="}, code: 1},
		{name: "failed", status: store.ResultStatusFailed, env: envURL, code: 1},
		{name: "done", status: store.ResultStatusDone, env: envURL},
		{name: "notify HTTP error", status: store.ResultStatusBlocked, env: envURL, wantURL: envURL, responseCode: http.StatusServiceUnavailable, code: 1, wantErr: "notify blocked task \"task-1\": blocked webhook returned 503 Service Unavailable: try later"},
		{name: "notify client error", status: store.ResultStatusBlocked, env: envURL, wantURL: envURL, clientErr: errors.New("network unavailable"), code: 1, wantErr: "notify blocked task \"task-1\": post blocked webhook"},
		{name: "agent error with blocked partial result", status: store.ResultStatusBlocked, env: envURL, agentErr: errors.New("agent stopped"), code: 1, wantErr: "agent stopped"},
		{name: "blocked with output error", status: store.ResultStatusBlocked, env: envURL, wantURL: envURL, code: 1, outputError: true, wantErr: "write result: output unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TROUPE_WEBHOOK_URL", tt.env)
			root := t.TempDir()
			taskGoal := "Implement a bounded task"
			result := store.Result{ID: "task-1", Status: tt.status, Summary: "Agent summary", Commit: "abc123", Notes: "Need a decision\n请确认"}
			a := fakeAgent{name: "codex", run: func(context.Context, store.Task, string) (store.Result, error) {
				return result, tt.agentErr
			}}
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != tt.wantURL || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
					t.Fatalf("unexpected request: %s %s, Content-Type = %q; want POST %s", r.Method, r.URL, r.Header.Get("Content-Type"), tt.wantURL)
				}
				var got map[string]string
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatal(err)
				}
				want := map[string]string{"task_id": "task-1", "status": "blocked", "summary": result.Summary, "notes": result.Notes, "goal": taskGoal}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("payload = %#v; want %#v", got, want)
				}
				if saved, err := openStore(t, root).GetTask(r.Context(), result.ID); err != nil || saved.Status != store.StatusBlocked {
					t.Fatalf("task at notification = %#v, %v; want persisted blocked task", saved, err)
				}
				if tt.clientErr != nil {
					return nil, tt.clientErr
				}
				w := httptest.NewRecorder()
				status := tt.responseCode
				if status == 0 {
					status = http.StatusNoContent
				}
				w.WriteHeader(status)
				if status >= 300 {
					io.WriteString(w, "try later")
				}
				return w.Result(), nil
			})}
			args := []string{"run", "--agent", a.Name(), "--goal", taskGoal, "--id", result.ID, "--store", root}
			args = append(args, tt.flags...)
			var stdout, stderr bytes.Buffer
			var output io.Writer = &stdout
			if tt.outputError {
				output = failingWriter{}
			}
			code := execute(context.Background(), args, output, &stderr, map[string]agent.Agent{a.Name(): a}, client)
			wantCalls := 0
			if tt.wantURL != "" {
				wantCalls = 1
			}
			if code != tt.code || calls != wantCalls {
				t.Fatalf("exit = %d, POSTs = %d; want %d, %d; stderr: %s", code, calls, tt.code, wantCalls, &stderr)
			}
			if tt.wantErr == "" && stderr.Len() != 0 || tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q; want error %q", &stderr, tt.wantErr)
			}
			if tt.clientErr != nil && !strings.Contains(stderr.String(), tt.clientErr.Error()) {
				t.Fatalf("stderr = %q; want client error", &stderr)
			}
			if !tt.outputError {
				wantOutput := "ID: task-1\nStatus: " + string(tt.status) + "\nSummary: Agent summary\nCommit: abc123\nNotes: " + result.Notes + "\n"
				if stdout.String() != wantOutput {
					t.Fatalf("stdout = %q; want %q", &stdout, wantOutput)
				}
			}
		})
	}
}

func TestResumeBlockedWebhook(t *testing.T) {
	for _, tt := range []struct {
		name     string
		envURL   bool
		flagURL  bool
		disabled bool
		wantPath string
	}{
		{name: "environment", envURL: true, wantPath: "/env"},
		{name: "flag overrides environment", envURL: true, flagURL: true, wantPath: "/flag"},
		{name: "empty flag disables environment", envURL: true, disabled: true},
		{name: "no URL"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			fs := openStore(t, root)
			task := store.Task{ID: "task-1", Goal: "Finish existing task", Status: store.StatusBlocked, Budget: store.Budget{MaxMinutes: 2}}
			if err := fs.SaveTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			type request struct {
				method, path, contentType string
				body                      []byte
				err                       error
			}
			requests := make(chan request, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				requests <- request{r.Method, r.URL.Path, r.Header.Get("Content-Type"), body, err}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			envURL := ""
			if tt.envURL {
				envURL = server.URL + "/env"
			}
			t.Setenv("TROUPE_WEBHOOK_URL", envURL)
			result := store.Result{ID: task.ID, Status: store.ResultStatusBlocked, Summary: "Need another decision", Notes: "请确认"}
			calls := 0
			a := fakeAgent{name: "codex", run: func(context.Context, store.Task, string) (store.Result, error) {
				calls++
				return result, nil
			}}
			args := []string{"resume", task.ID, "--agent", a.Name(), "--store", root}
			if tt.flagURL {
				args = append(args, "--webhook-url", server.URL+"/flag")
			}
			if tt.disabled {
				args = append(args, "--webhook-url=")
			}
			var stdout, stderr bytes.Buffer
			if code := execute(ctx, args, &stdout, &stderr, map[string]agent.Agent{a.Name(): a}, server.Client()); code != 1 || calls != 1 || stderr.Len() != 0 {
				t.Fatalf("exit = %d, agent calls = %d, stderr = %q; want 1, 1 and no error", code, calls, &stderr)
			}
			if saved, err := fs.GetTask(ctx, task.ID); err != nil || saved.Status != store.StatusBlocked {
				t.Fatalf("task after resume = %#v, %v; want blocked", saved, err)
			}
			wantOutput := "ID: task-1\nStatus: blocked\nSummary: Need another decision\nCommit: \nNotes: 请确认\n"
			if stdout.String() != wantOutput {
				t.Fatalf("stdout = %q; want %q", &stdout, wantOutput)
			}
			wantCalls := 0
			if tt.wantPath != "" {
				wantCalls = 1
			}
			if len(requests) != wantCalls {
				t.Fatalf("webhook POSTs = %d; want %d", len(requests), wantCalls)
			}
			if wantCalls == 0 {
				return
			}
			got := <-requests
			if got.err != nil || got.method != http.MethodPost || got.path != tt.wantPath || got.contentType != "application/json" {
				t.Fatalf("unexpected webhook request: %#v", got)
			}
			var payload map[string]string
			if err := json.Unmarshal(got.body, &payload); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"task_id": task.ID, "status": "blocked", "summary": result.Summary, "notes": result.Notes, "goal": task.Goal}
			if !reflect.DeepEqual(payload, want) {
				t.Fatalf("payload = %#v; want %#v", payload, want)
			}
		})
	}
}

func openStore(t *testing.T, root string) *store.FileStore {
	t.Helper()
	fs, err := store.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}
