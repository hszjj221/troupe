package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hszjj221/troupe/internal/agent"
	"github.com/hszjj221/troupe/internal/config"
	"github.com/hszjj221/troupe/internal/store"
)

func TestRunConfig(t *testing.T) {
	for _, tt := range []struct {
		name, yaml, path, env, home string
		flags                       []string
		agent, role, wantErr        string
	}{
		{name: "role binding", yaml: "default_agent: codex\nroles:\n  coder: claude\n", flags: []string{"--role", "coder"}, agent: "claude", role: "coder"},
		{name: "explicit config beats environment", path: "custom.yaml", env: "missing.yaml", yaml: "default_agent: claude\n", flags: []string{"--config", "custom.yaml"}, agent: "claude"},
		{name: "agent overrides binding", yaml: "roles:\n  coder: claude\nagents:\n  codex:\n    binary: /unused/codex\n", flags: []string{"--agent", "codex", "--role", "coder"}, agent: "codex", role: "coder"},
		{name: "unbound role uses default", yaml: "default_agent: claude\n", flags: []string{"--role", "reviewer"}, agent: "claude", role: "reviewer"},
		{name: "environment config", path: "env.yaml", env: "env.yaml", yaml: "default_agent: claude\n", agent: "claude"},
		{name: "home config independent of store", path: "config-home/config.yaml", home: "config-home", yaml: "default_agent: claude\n", agent: "claude"},
		{name: "default home config", path: ".troupe/config.yaml", yaml: "default_agent: codex\n", agent: "codex"},
		{name: "no resolvable agent", wantErr: "need --agent or config"},
		{name: "unknown binding", yaml: "default_agent: codex\nroles:\n  coder: missing\n", flags: []string{"--role", "coder"}, wantErr: `unknown agent "missing"; choose codex or claude`},
		{name: "unknown default", yaml: "default_agent: missing\n", wantErr: `unknown agent "missing"; choose codex or claude`},
		{name: "explicit missing does not fall back", yaml: "default_agent: codex\n", flags: []string{"--agent", "codex", "--config", "missing.yaml"}, wantErr: `read config "missing.yaml"`},
		{name: "environment missing does not fall back", env: "missing.yaml", yaml: "default_agent: codex\n", flags: []string{"--agent", "codex"}, wantErr: `read config "missing.yaml"`},
		{name: "malformed config with explicit agent", yaml: "roles: [", flags: []string{"--agent", "codex"}, wantErr: "decode config"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("TROUPE_CONFIG", tt.env)
			t.Setenv("TROUPE_HOME", tt.home)
			t.Setenv("TROUPE_WEBHOOK_URL", "")
			path := tt.path
			if path == "" {
				path = "troupe.yaml"
			}
			if tt.yaml != "" {
				writeConfig(t, path, tt.yaml)
			}
			calls := 0
			agents := map[string]agent.Agent{}
			for _, name := range []string{"codex", "claude"} {
				agents[name] = fakeAgent{name: name, run: func(_ context.Context, task store.Task, _ string) (store.Result, error) {
					calls++
					if name != tt.agent || task.Role != tt.role {
						t.Fatalf("agent = %q, role = %q; want %q, %q", name, task.Role, tt.agent, tt.role)
					}
					return store.Result{ID: task.ID, Status: store.ResultStatusDone}, nil
				}}
			}
			args := append([]string{"run", "--goal", "task", "--id", "task-1", "--store", "state"}, tt.flags...)
			var stdout, stderr bytes.Buffer
			code := execute(context.Background(), args, &stdout, &stderr, agents, nil)
			if tt.wantErr != "" {
				if code != 1 || calls != 0 || !strings.Contains(stderr.String(), tt.wantErr) || stdout.Len() != 0 {
					t.Fatalf("exit = %d, calls = %d, stdout = %q, stderr = %q; want 1, 0 and %q", code, calls, &stdout, &stderr, tt.wantErr)
				}
				if _, err := os.Stat("state"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid config created store: %v", err)
				}
				return
			}
			if code != 0 || calls != 1 || stderr.Len() != 0 {
				t.Fatalf("exit = %d, calls = %d, stderr = %q; want 0, 1 and no error", code, calls, &stderr)
			}
			if saved, err := openStore(t, "state").GetTask(context.Background(), "task-1"); err != nil || saved.Role != tt.role || saved.Status != store.StatusDone {
				t.Fatalf("saved task = %#v, %v; want role %q and done", saved, err, tt.role)
			}
		})
	}
}

func TestResumeConfig(t *testing.T) {
	const bindings = "default_agent: codex\nroles:\n  coder: claude\n  reviewer: codex\n"
	for _, tt := range []struct {
		name, yaml           string
		flags                []string
		beforeID             bool
		agent, role, wantErr string
	}{
		{name: "stored role", yaml: bindings, agent: "claude", role: "coder"},
		{name: "role override before ID", yaml: bindings, flags: []string{"--role", "reviewer"}, beforeID: true, agent: "codex", role: "reviewer"},
		{name: "role override after ID", yaml: bindings, flags: []string{"--role", "reviewer"}, agent: "codex", role: "reviewer"},
		{name: "empty role override", yaml: bindings, flags: []string{"--role="}, agent: "codex"},
		{name: "agent overrides stored binding", yaml: bindings, flags: []string{"--agent", "codex"}, agent: "codex", role: "coder"},
		{name: "agent and role override", yaml: bindings, flags: []string{"--agent", "claude", "--role", "reviewer"}, agent: "claude", role: "reviewer"},
		{name: "default agent", yaml: "default_agent: codex\n", agent: "codex", role: "coder"},
		{name: "explicit config", yaml: bindings, flags: []string{"--config", "troupe.yaml"}, agent: "claude", role: "coder"},
		{name: "no resolvable agent", wantErr: "need --agent or config"},
		{name: "unknown explicit agent", flags: []string{"--agent", "missing"}, wantErr: `unknown agent "missing"; choose codex or claude`},
		{name: "unknown binding", yaml: "roles:\n  coder: missing\n", flags: []string{"--role", "coder"}, wantErr: `unknown agent "missing"; choose codex or claude`},
		{name: "explicit missing", yaml: bindings, flags: []string{"--config", "missing.yaml"}, wantErr: `read config "missing.yaml"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("TROUPE_CONFIG", "")
			t.Setenv("TROUPE_HOME", "")
			t.Setenv("TROUPE_WEBHOOK_URL", "")
			if tt.yaml != "" {
				writeConfig(t, "troupe.yaml", tt.yaml)
			}
			ctx := context.Background()
			fs := openStore(t, "state")
			task := store.Task{ID: "task-1", Goal: "task", Role: "coder", Context: "Keep context", Status: store.StatusBlocked, Budget: store.Budget{MaxMinutes: 2}}
			if err := fs.SaveTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			calls := 0
			agents := map[string]agent.Agent{}
			for _, name := range []string{"codex", "claude"} {
				agents[name] = fakeAgent{name: name, run: func(ctx context.Context, got store.Task, _ string) (store.Result, error) {
					calls++
					if name != tt.agent || got.Role != tt.role {
						t.Fatalf("agent = %q, role = %q; want %q, %q", name, got.Role, tt.agent, tt.role)
					}
					if saved, err := fs.GetTask(ctx, task.ID); err != nil || saved.Role != tt.role {
						t.Fatalf("role not saved before Run: %#v, %v", saved, err)
					}
					return store.Result{ID: got.ID, Status: store.ResultStatusDone}, nil
				}}
			}
			args := []string{"resume", "--store", "state"}
			if tt.beforeID {
				args = append(args, tt.flags...)
				args = append(args, task.ID)
			} else {
				args = append(args, task.ID)
				args = append(args, tt.flags...)
			}
			var stdout, stderr bytes.Buffer
			code := execute(ctx, args, &stdout, &stderr, agents, nil)
			wantTask := task
			if tt.wantErr != "" {
				if code != 1 || calls != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), tt.wantErr) {
					t.Fatalf("exit = %d, calls = %d, stdout = %q, stderr = %q; want 1, 0 and %q", code, calls, &stdout, &stderr, tt.wantErr)
				}
			} else {
				if code != 0 || calls != 1 || stderr.Len() != 0 {
					t.Fatalf("exit = %d, calls = %d, stderr = %q; want 0, 1 and no error", code, calls, &stderr)
				}
				wantTask.Role, wantTask.Status = tt.role, store.StatusDone
			}
			if saved, err := fs.GetTask(ctx, task.ID); err != nil || !reflect.DeepEqual(saved, wantTask) {
				t.Fatalf("saved task = %#v, %v; want %#v", saved, err, wantTask)
			}
		})
	}
}

func TestAgentsFromConfigBinaries(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{Agents: map[string]config.AgentConfig{
		"codex":   {Binary: filepath.Join(root, "missing-codex")},
		"claude":  {Binary: filepath.Join(root, "missing-claude")},
		"unknown": {Binary: "unused"},
	}}
	agents := agentsFromConfig(cfg)
	if len(agents) != 2 {
		t.Fatalf("agents = %v; want only codex and claude", agents)
	}
	for _, name := range []string{"codex", "claude"} {
		// A nonexistent path verifies the binary actually used without launching
		// any real agent executable.
		_, err := agents[name].Run(context.Background(), store.Task{}, t.TempDir())
		want := fmt.Sprintf("%s exec %q", name, cfg.Agents[name].Binary)
		if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s Run error = %v; want missing binary %q", name, err, want)
		}
	}
}

func writeConfig(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
