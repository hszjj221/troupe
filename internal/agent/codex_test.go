package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hszjj221/troupe/internal/store"
	"gopkg.in/yaml.v3"
)

func TestCodexName(t *testing.T) {
	if got := NewCodex(CodexOptions{}).Name(); got != "codex" {
		t.Fatalf("Name = %q, want codex", got)
	}
}

func TestCodexRun(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "new-run")
	task := store.Task{
		ID: "task-1", Goal: "Fix a bug", Role: "coder", Workdir: t.TempDir(),
		Acceptance: []string{"go test ./..."}, Context: "Keep the change small.",
		Budget: store.Budget{MaxMinutes: 5}, Status: store.StatusRunning,
	}
	want := store.Result{
		ID: task.ID, Status: store.ResultStatusDone, Summary: "Fixed the bug",
		Commit: "abc123", Notes: "Tests passed",
	}
	c := NewCodex(CodexOptions{Runner: func(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
		if name != "codex" {
			t.Fatalf("binary = %q, want codex", name)
		}
		wantArgs := []string{"exec", "--sandbox", "workspace-write", "--add-dir", runDir, "--cd", task.Workdir, "--"}
		if len(args) != len(wantArgs)+1 || !reflect.DeepEqual(args[:len(wantArgs)], wantArgs) {
			t.Fatalf("args = %#v, want %#v followed by prompt", args, wantArgs)
		}
		for _, text := range []string{task.Goal, task.Context, filepath.Join(runDir, "task.yaml"), filepath.Join(runDir, "result.json")} {
			if !strings.Contains(args[len(args)-1], text) {
				t.Errorf("prompt missing %q", text)
			}
		}
		data, err := os.ReadFile(filepath.Join(runDir, "task.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var saved store.Task
		if err := yaml.Unmarshal(data, &saved); err != nil || !reflect.DeepEqual(saved, task) {
			t.Fatalf("task.yaml = %#v, %v; want %#v", saved, err, task)
		}
		if _, err := io.WriteString(stdout, "stdout output\n"); err != nil {
			return err
		}
		if _, err := io.WriteString(stderr, "stderr output\n"); err != nil {
			return err
		}
		data, err = json.Marshal(want)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(runDir, "result.json"), data, 0o644)
	}})
	got, err := c.Run(context.Background(), task, runDir)
	if err != nil || got != want {
		t.Fatalf("Run = %#v, %v; want %#v", got, err, want)
	}
	for _, stream := range []string{"stdout", "stderr"} {
		data, err := os.ReadFile(filepath.Join(runDir, "logs", "codex."+stream+".log"))
		if err != nil || string(data) != stream+" output\n" {
			t.Errorf("%s log = %q, %v", stream, data, err)
		}
	}
}

func TestCodexResultErrors(t *testing.T) {
	for _, tt := range []struct {
		name, data, message string
	}{
		{"missing", "", "read result.json"},
		{"malformed", "{", "decode result.json"},
		{"wrong type", "[]", "decode result.json"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runDir := t.TempDir()
			c := NewCodex(CodexOptions{Runner: func(context.Context, string, []string, io.Writer, io.Writer) error {
				if tt.data == "" {
					return nil
				}
				return os.WriteFile(filepath.Join(runDir, "result.json"), []byte(tt.data), 0o644)
			}})
			_, err := c.Run(context.Background(), store.Task{}, runDir)
			if err == nil || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("Run error = %v, want %q", err, tt.message)
			}
			if tt.data == "" && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Run error = %v, want os.ErrNotExist", err)
			}
		})
	}
}

func TestCodexContext(t *testing.T) {
	for _, tt := range []struct {
		name string
		want error
	}{
		{"cancel", context.Canceled},
		{"timeout", context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if tt.want == context.Canceled {
				ctx, cancel = context.WithCancel(context.Background())
			} else {
				ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
			}
			defer cancel()
			c := NewCodex(CodexOptions{Runner: func(ctx context.Context, _ string, _ []string, _, _ io.Writer) error {
				if tt.want == context.Canceled {
					cancel()
				}
				<-ctx.Done()
				// Real subprocesses may report a signal instead of ctx.Err().
				return errors.New("process killed")
			}})
			_, err := c.Run(ctx, store.Task{}, t.TempDir())
			if !errors.Is(err, tt.want) {
				t.Fatalf("Run error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCodexAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := NewCodex(CodexOptions{Runner: func(context.Context, string, []string, io.Writer, io.Writer) error {
		t.Fatal("runner called with canceled context")
		return nil
	}})
	if _, err := c.Run(ctx, store.Task{}, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
}

func TestCodexMissingBinary(t *testing.T) {
	c := NewCodex(CodexOptions{Binary: filepath.Join(t.TempDir(), "missing-codex")})
	_, err := c.Run(context.Background(), store.Task{}, t.TempDir())
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "missing-codex") {
		t.Fatalf("Run error = %v, want missing binary error", err)
	}
}

func TestCodexCommandFailure(t *testing.T) {
	runDir := t.TempDir()
	wantErr := errors.New("exit status 1")
	c := NewCodex(CodexOptions{Runner: func(_ context.Context, _ string, _ []string, _, stderr io.Writer) error {
		if _, err := io.WriteString(stderr, "command failed"); err != nil {
			return err
		}
		return wantErr
	}})
	if _, err := c.Run(context.Background(), store.Task{}, runDir); !errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want %v", err, wantErr)
	}
	data, err := os.ReadFile(filepath.Join(runDir, "logs", "codex.stderr.log"))
	if err != nil || string(data) != "command failed" {
		t.Fatalf("stderr log = %q, %v", data, err)
	}
}

func TestCodexInvalidRunDir(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(runDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCodex(CodexOptions{Runner: func(context.Context, string, []string, io.Writer, io.Writer) error {
		t.Fatal("runner called with invalid run directory")
		return nil
	}})
	_, err := c.Run(context.Background(), store.Task{}, runDir)
	if err == nil || !strings.Contains(err.Error(), "create run directory") {
		t.Fatalf("Run error = %v, want run directory error", err)
	}
}
