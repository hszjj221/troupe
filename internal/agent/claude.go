package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/hszjj221/troupe/internal/store"
	"gopkg.in/yaml.v3"
)

// ClaudeOptions configures the CLI binary and permits a runner without Claude in tests.
type ClaudeOptions struct {
	Binary string
	Runner Runner
}

// Claude is a thin adapter for the non-interactive Claude Code CLI.
type Claude struct {
	binary string
	runner Runner
}

var _ Agent = (*Claude)(nil)

// NewClaude defaults to the claude binary on PATH and exec.CommandContext.
func NewClaude(opts ClaudeOptions) *Claude {
	if opts.Binary == "" {
		opts.Binary = "claude"
	}
	if opts.Runner == nil {
		opts.Runner = func(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stdout, cmd.Stderr = stdout, stderr
			return cmd.Run()
		}
	}
	return &Claude{binary: opts.Binary, runner: opts.Runner}
}

func (c *Claude) Name() string { return "claude" }

// Run writes task.yaml, captures logs, and reads result.json after a successful
// command. Callers supply a fresh directory for each run and control the timeout
// through ctx; task.Budget is left to the caller.
func (c *Claude) Run(ctx context.Context, task store.Task, runDir string) (store.Result, error) {
	if err := ctx.Err(); err != nil {
		return store.Result{}, fmt.Errorf("claude: %w", err)
	}
	runDir, err := filepath.Abs(runDir)
	if err != nil {
		return store.Result{}, fmt.Errorf("claude: resolve run directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(runDir, "logs"), 0o755); err != nil {
		return store.Result{}, fmt.Errorf("claude: create run directory: %w", err)
	}
	taskData, err := yaml.Marshal(task)
	if err != nil {
		return store.Result{}, fmt.Errorf("claude: encode task.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "task.yaml"), taskData, 0o644); err != nil {
		return store.Result{}, fmt.Errorf("claude: write task.yaml: %w", err)
	}
	stdout, err := os.Create(filepath.Join(runDir, "logs", "claude.stdout.log"))
	if err != nil {
		return store.Result{}, fmt.Errorf("claude: create stdout log: %w", err)
	}
	defer stdout.Close()
	stderr, err := os.Create(filepath.Join(runDir, "logs", "claude.stderr.log"))
	if err != nil {
		return store.Result{}, fmt.Errorf("claude: create stderr log: %w", err)
	}
	defer stderr.Close()

	resultPath := filepath.Join(runDir, "result.json")
	prompt := fmt.Sprintf("Complete this task (archived in %q). Write a JSON object to %q before exiting with fields id, status (done/failed/blocked), summary, commit, and notes.\n\n%s",
		filepath.Join(runDir, "task.yaml"), resultPath, taskData)
	args := []string{"-p", "--add-dir", runDir}
	if task.Workdir != "" {
		args = append(args, "--add-dir", task.Workdir)
	}
	args = append(args, "--", prompt)
	err = c.runner(ctx, c.binary, args, stdout, stderr)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return store.Result{}, fmt.Errorf("claude exec: %w", ctxErr)
	}
	if err != nil {
		return store.Result{}, fmt.Errorf("claude exec %q: %w", c.binary, err)
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return store.Result{}, fmt.Errorf("claude: read result.json: %w", err)
	}
	var result store.Result
	if err := json.Unmarshal(data, &result); err != nil {
		return store.Result{}, fmt.Errorf("claude: decode result.json: %w", err)
	}
	return result, nil
}
