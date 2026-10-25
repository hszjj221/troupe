package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hszjj221/troupe/internal/agent"
	"github.com/hszjj221/troupe/internal/notify"
	"github.com/hszjj221/troupe/internal/orchestrator"
	"github.com/hszjj221/troupe/internal/store"
)

const usage = "Usage: troupe run --agent <codex|claude> --goal <goal> [flags]\nRun 'troupe run --help' for flags."

// Execute runs args (excluding the executable name) and returns a process exit
// code. It does not exit the process; callers control cancellation and output.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return execute(ctx, args, stdout, stderr, defaultAgents(), nil)
}

func defaultAgents() map[string]agent.Agent {
	codex := agent.NewCodex(agent.CodexOptions{})
	claude := agent.NewClaude(agent.ClaudeOptions{})
	return map[string]agent.Agent{codex.Name(): codex, claude.Name(): claude}
}

func execute(ctx context.Context, args []string, stdout, stderr io.Writer, agents map[string]agent.Agent, client *http.Client) int {
	if len(args) == 0 {
		code := fail(stderr, "expected run subcommand")
		fmt.Fprintln(stderr, usage)
		return code
	}
	switch args[0] {
	case "-h", "--help", "help":
		if len(args) != 1 {
			return fail(stderr, "unexpected arguments after %q", args[0])
		}
		if _, err := fmt.Fprintln(stdout, usage); err != nil {
			return fail(stderr, "write help: %v", err)
		}
		return 0
	case "run":
		return run(ctx, args[1:], stdout, stderr, agents, client)
	default:
		code := fail(stderr, "unknown subcommand %q", args[0])
		fmt.Fprintln(stderr, usage)
		return code
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, agents map[string]agent.Agent, client *http.Client) int {
	root := os.Getenv("TROUPE_HOME")
	if root == "" {
		root = ".troupe"
	}
	var task store.Task
	var agentName string
	var webhookURL string
	flags := flag.NewFlagSet("troupe run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: troupe run --agent <codex|claude> --goal <goal> [flags]")
		flags.PrintDefaults()
	}
	flags.StringVar(&task.Goal, "goal", "", "task goal (required)")
	flags.StringVar(&agentName, "agent", "", "agent: codex or claude (required)")
	flags.StringVar(&task.ID, "id", "", "task ID (default random t- prefixed ID; must be new)")
	flags.StringVar(&task.Role, "role", "", "optional task role, independent of agent selection")
	flags.StringVar(&task.Workdir, "workdir", ".", "working directory (default current directory)")
	flags.StringVar(&task.Context, "context", "", "supplementary task context")
	flags.Func("acceptance", "acceptance command (repeatable; one command per flag)", func(command string) error {
		if strings.TrimSpace(command) == "" {
			return errors.New("acceptance command must not be empty")
		}
		task.Acceptance = append(task.Acceptance, command)
		return nil
	})
	flags.IntVar(&task.Budget.MaxMinutes, "budget-minutes", 30, "positive agent timeout in minutes")
	flags.StringVar(&root, "store", root, "store root (flag > TROUPE_HOME > .troupe)")
	flags.StringVar(&webhookURL, "webhook-url", os.Getenv("TROUPE_WEBHOOK_URL"), "blocked webhook URL (flag > TROUPE_WEBHOOK_URL > empty/disabled; empty flag disables)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1 // flag already printed the error and usage.
	}
	invalid := func(format string, args ...any) int {
		code := fail(stderr, format, args...)
		flags.Usage()
		return code
	}
	if flags.NArg() != 0 {
		return invalid("unexpected positional arguments; use --goal")
	}
	if strings.TrimSpace(task.Goal) == "" {
		return invalid("--goal is required")
	}
	if agentName == "" {
		return invalid("--agent is required (codex or claude)")
	}
	if (agentName != "codex" && agentName != "claude") || agents[agentName] == nil {
		return invalid("unknown agent %q; choose codex or claude", agentName)
	}
	const maxBudgetMinutes = int64((1<<63 - 1) / time.Minute)
	if task.Budget.MaxMinutes <= 0 || int64(task.Budget.MaxMinutes) > maxBudgetMinutes {
		return invalid("--budget-minutes must be between 1 and %d", maxBudgetMinutes)
	}
	if strings.TrimSpace(root) == "" {
		return invalid("--store must not be empty")
	}
	var err error
	root, err = filepath.Abs(root)
	if err != nil {
		return fail(stderr, "resolve store root: %v", err)
	}
	task.Workdir, err = filepath.Abs(task.Workdir)
	if err != nil {
		return fail(stderr, "resolve workdir: %v", err)
	}
	if task.ID == "" {
		task.ID = "t-" + rand.Text()
	}
	task.Status = store.StatusPending
	fs, err := store.NewFileStore(root)
	if err != nil {
		return fail(stderr, "open store: %v", err)
	}
	if _, err := fs.GetTask(ctx, task.ID); err == nil {
		return fail(stderr, "task %q already exists; choose a new --id", task.ID)
	} else if !errors.Is(err, store.ErrNotFound) {
		return fail(stderr, "check task %q: %v", task.ID, err)
	}
	if err := fs.SaveTask(ctx, task); err != nil {
		return fail(stderr, "save task %q: %v", task.ID, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(task.Budget.MaxMinutes)*time.Minute)
	defer cancel()
	result, runErr := orchestrator.New(fs, agents, filepath.Join(root, "runs")).Run(runCtx, task.ID, agentName)
	var outputErr error
	if result != (store.Result{}) {
		if _, err := fmt.Fprintf(stdout, "ID: %s\nStatus: %s\nSummary: %s\nCommit: %s\nNotes: %s\n",
			result.ID, result.Status, result.Summary, result.Commit, result.Notes); err != nil {
			fail(stderr, "write result: %v", err)
			outputErr = err
		}
	}
	if runErr != nil {
		return fail(stderr, "run task %q: %v", task.ID, runErr)
	}
	if result.Status == store.ResultStatusBlocked {
		n := notify.Notifier{URL: webhookURL, Client: client}
		if err := n.NotifyBlocked(ctx, task, result); err != nil {
			fail(stderr, "notify blocked task %q: %v", task.ID, err)
		}
	}
	if outputErr != nil || result.Status != store.ResultStatusDone {
		return 1
	}
	return 0
}

func fail(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "troupe: "+format+"\n", args...)
	return 1
}
