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

func TestValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		err  string
	}{
		{"no subcommand", nil, "expected run subcommand"},
		{"unknown subcommand", []string{"resume"}, `unknown subcommand "resume"`},
		{"missing goal and agent", []string{"run"}, "--goal is required"},
		{"missing goal", []string{"run", "--agent", "codex"}, "--goal is required"},
		{"blank goal", []string{"run", "--goal", "  ", "--agent", "codex"}, "--goal is required"},
		{"missing agent", []string{"run", "--goal", "task"}, "--agent is required"},
		{"unknown agent", []string{"run", "--goal", "task", "--agent", "missing"}, `unknown agent "missing"`},
		{"positional goal", []string{"run", "--agent", "codex", "task"}, "use --goal"},
		{"unknown flag", []string{"run", "--unknown"}, "flag provided but not defined"},
		{"missing flag value", []string{"run", "--goal"}, "flag needs an argument"},
		{"zero budget", []string{"run", "--goal", "task", "--agent", "codex", "--budget-minutes", "0"}, "--budget-minutes must be between"},
		{"negative budget", []string{"run", "--goal", "task", "--agent", "codex", "--budget-minutes", "-1"}, "--budget-minutes must be between"},
		{"overflowing budget", []string{"run", "--goal", "task", "--agent", "codex", "--budget-minutes", "153722868"}, "--budget-minutes must be between"},
		{"empty acceptance", []string{"run", "--goal", "task", "--agent", "codex", "--acceptance", " "}, "acceptance command must not be empty"},
		{"empty store", []string{"run", "--goal", "task", "--agent", "codex", "--store", ""}, "--store must not be empty"},
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
				if want := filepath.Join(root, "runs", "task-1", "1"); dir != want {
					t.Fatalf("run directory = %q; want %q", dir, want)
				}
				return store.Result{ID: task.ID, Status: store.ResultStatusDone}, nil
			}}
			args := []string{"run", "--goal", "task", "--agent", "codex", "--id", "task-1"}
			if tt.flag != "" {
				args = append(args, "--store", tt.flag)
			}
			var stdout, stderr bytes.Buffer
			if code := execute(context.Background(), args, &stdout, &stderr, map[string]agent.Agent{a.Name(): a}, nil); code != 0 {
				t.Fatalf("exit = %d; stderr: %s", code, &stderr)
			}
			if task, err := openStore(t, root).GetTask(context.Background(), "task-1"); err != nil || task.Status != store.StatusDone {
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
	for _, args := range [][]string{{"--help"}, {"run", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Execute(context.Background(), args, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String()+stderr.String(), "Usage:") {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q; want help and exit 0", code, &stdout, &stderr)
			}
		})
	}
	registered := defaultAgents()
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

func openStore(t *testing.T, root string) *store.FileStore {
	t.Helper()
	fs, err := store.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}
