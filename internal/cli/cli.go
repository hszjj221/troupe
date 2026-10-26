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
	"sort"
	"strings"
	"time"

	"github.com/hszjj221/troupe/internal/agent"
	"github.com/hszjj221/troupe/internal/config"
	"github.com/hszjj221/troupe/internal/notify"
	"github.com/hszjj221/troupe/internal/orchestrator"
	"github.com/hszjj221/troupe/internal/store"
)

const usage = "Usage:\n  troupe run --goal <goal> [--agent <codex|claude>] [--config <path>] [flags]\n  troupe resume <id> [--agent <codex|claude>] [--config <path>] [flags]\n  troupe status <id> [--store <root>]\n  troupe list [--status pending|running|blocked|done|failed] [--store <root>]\nAgent selection: --agent > config role binding > default_agent.\nRun 'troupe <command> --help' for flags (run, resume, status, list)."

const maxBudgetMinutes = int64((1<<63 - 1) / time.Minute)

// Execute runs args (excluding the executable name) and returns a process exit
// code. It does not exit the process; callers control cancellation and output.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return execute(ctx, args, stdout, stderr, nil, nil)
}

func agentsFromConfig(cfg config.Config) map[string]agent.Agent {
	codex := agent.NewCodex(agent.CodexOptions{Binary: cfg.Agents["codex"].Binary})
	claude := agent.NewClaude(agent.ClaudeOptions{Binary: cfg.Agents["claude"].Binary})
	return map[string]agent.Agent{codex.Name(): codex, claude.Name(): claude}
}

func loadConfig(path string) (config.Config, error) {
	return config.Load(config.ResolvePath(path, os.Getenv("TROUPE_CONFIG"), os.Getenv("TROUPE_HOME")))
}

func execute(ctx context.Context, args []string, stdout, stderr io.Writer, agents map[string]agent.Agent, client *http.Client) int {
	if len(args) == 0 {
		code := fail(stderr, "expected run, resume, status, or list subcommand")
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
	case "resume":
		return resume(ctx, args[1:], stdout, stderr, agents, client)
	case "status":
		return status(ctx, args[1:], stdout, stderr)
	case "list":
		return list(ctx, args[1:], stdout, stderr)
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
	var agentName, configPath string
	var webhookURL string
	flags := flag.NewFlagSet("troupe run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: troupe run --goal <goal> [--agent <codex|claude>] [--config <path>] [flags]")
		flags.PrintDefaults()
	}
	flags.StringVar(&task.Goal, "goal", "", "task goal (required)")
	flags.StringVar(&agentName, "agent", "", "agent override: codex or claude (otherwise role binding, then default_agent)")
	flags.StringVar(&configPath, "config", "", "YAML config (flag > TROUPE_CONFIG > ./troupe.yaml > TROUPE_HOME/config.yaml or .troupe/config.yaml)")
	flags.StringVar(&task.ID, "id", "", "task ID (default random t- prefixed ID; must be new)")
	flags.StringVar(&task.Role, "role", "", "task role; selects config role binding unless --agent is given")
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
	cfg, err := loadConfig(configPath)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	if agents == nil {
		agents = agentsFromConfig(cfg)
	}
	agentName, err = config.ResolveAgent(cfg, agentName, task.Role)
	if err != nil {
		return invalid("%v", err)
	}
	if (agentName != "codex" && agentName != "claude") || agents[agentName] == nil {
		return invalid("unknown agent %q; choose codex or claude", agentName)
	}
	if task.Budget.MaxMinutes <= 0 || int64(task.Budget.MaxMinutes) > maxBudgetMinutes {
		return invalid("--budget-minutes must be between 1 and %d", maxBudgetMinutes)
	}
	if strings.TrimSpace(root) == "" {
		return invalid("--store must not be empty")
	}
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
	return runTask(ctx, task, agentName, fs, root, webhookURL, stdout, stderr, agents, client)
}

func resume(ctx context.Context, args []string, stdout, stderr io.Writer, agents map[string]agent.Agent, client *http.Client) int {
	root := os.Getenv("TROUPE_HOME")
	if root == "" {
		root = ".troupe"
	}
	var agentName, role, configPath, note, webhookURL string
	flags := flag.NewFlagSet("troupe resume", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: troupe resume <id> [--agent <codex|claude>] [--config <path>] [flags]")
		flags.PrintDefaults()
	}
	flags.StringVar(&agentName, "agent", "", "agent override: codex or claude (otherwise role binding, then default_agent)")
	flags.StringVar(&configPath, "config", "", "YAML config (flag > TROUPE_CONFIG > ./troupe.yaml > TROUPE_HOME/config.yaml or .troupe/config.yaml)")
	flags.StringVar(&role, "role", "", "task role override (default stored role); selects config binding unless --agent is given")
	flags.StringVar(&note, "note", "", "user reply to append to task context before resuming")
	flags.StringVar(&root, "store", root, "store root (flag > TROUPE_HOME > .troupe)")
	flags.StringVar(&webhookURL, "webhook-url", os.Getenv("TROUPE_WEBHOOK_URL"), "blocked webhook URL (flag > TROUPE_WEBHOOK_URL > empty/disabled; empty flag disables)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	invalid := func(format string, args ...any) int {
		code := fail(stderr, format, args...)
		flags.Usage()
		return code
	}
	if flags.NArg() == 0 {
		return invalid("task ID is required")
	}
	taskID := flags.Arg(0)
	// Standard flags stop at the first positional argument. Parse the tail as
	// well so flags can appear before or after the single task ID.
	if err := flags.Parse(flags.Args()[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if flags.NArg() != 0 {
		return invalid("expected exactly one task ID; unexpected positional arguments")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	if agents == nil {
		agents = agentsFromConfig(cfg)
	}
	if strings.TrimSpace(root) == "" {
		return invalid("--store must not be empty")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return fail(stderr, "resolve store root: %v", err)
	}
	fs, err := store.NewFileStore(root)
	if err != nil {
		return fail(stderr, "open store: %v", err)
	}
	task, err := fs.GetTask(ctx, taskID)
	if err != nil {
		return fail(stderr, "load task %q: %v", taskID, err)
	}
	if task.Status != store.StatusBlocked {
		return fail(stderr, "cannot resume task %q: current status is %q; expected blocked", taskID, task.Status)
	}
	if task.Budget.MaxMinutes <= 0 || int64(task.Budget.MaxMinutes) > maxBudgetMinutes {
		return fail(stderr, "task %q budget max_minutes must be between 1 and %d", taskID, maxBudgetMinutes)
	}
	noteSet, roleSet := false, false
	flags.Visit(func(f *flag.Flag) {
		noteSet = noteSet || f.Name == "note"
		roleSet = roleSet || f.Name == "role"
	})
	if roleSet {
		task.Role = role
	}
	agentName, err = config.ResolveAgent(cfg, agentName, task.Role)
	if err != nil {
		return invalid("%v", err)
	}
	if (agentName != "codex" && agentName != "claude") || agents[agentName] == nil {
		return invalid("unknown agent %q; choose codex or claude", agentName)
	}
	// Save replies and role overrides while still blocked, before handing the
	// pending task to Run.
	if noteSet {
		if task.Context != "" {
			task.Context += "\n\n"
		}
		task.Context += "User reply: " + note
	}
	if noteSet || roleSet {
		if err := fs.SaveTask(ctx, task); err != nil {
			return fail(stderr, "save task %q: %v", taskID, err)
		}
	}
	if err := fs.UpdateStatus(ctx, taskID, store.StatusBlocked, store.StatusPending); err != nil {
		return fail(stderr, "unblock task %q: %v", taskID, err)
	}
	task.Status = store.StatusPending
	return runTask(ctx, task, agentName, fs, root, webhookURL, stdout, stderr, agents, client)
}

func status(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := os.Getenv("TROUPE_HOME")
	if root == "" {
		root = ".troupe"
	}
	flags := flag.NewFlagSet("troupe status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: troupe status <id> [--store <root>]")
		flags.PrintDefaults()
	}
	flags.StringVar(&root, "store", root, "store root (flag > TROUPE_HOME > .troupe)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	invalid := func(format string, args ...any) int {
		code := fail(stderr, format, args...)
		flags.Usage()
		return code
	}
	if flags.NArg() == 0 {
		return invalid("task ID is required")
	}
	taskID := flags.Arg(0)
	// As with resume, allow flags before or after the single task ID.
	if err := flags.Parse(flags.Args()[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if flags.NArg() != 0 {
		return invalid("expected exactly one task ID; unexpected positional arguments")
	}
	if strings.TrimSpace(root) == "" {
		return invalid("--store must not be empty")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return fail(stderr, "resolve store root: %v", err)
	}
	fs, err := store.NewFileStore(root)
	if err != nil {
		return fail(stderr, "open store: %v", err)
	}
	task, err := fs.GetTask(ctx, taskID)
	if err != nil {
		return fail(stderr, "load task %q: %v", taskID, err)
	}
	if _, err := fmt.Fprintf(stdout, "ID: %s\nStatus: %s\nGoal: %s\nRole: %s\nWorkdir: %s\n",
		task.ID, task.Status, task.Goal, task.Role, task.Workdir); err != nil {
		return fail(stderr, "write task: %v", err)
	}
	return 0
}

func list(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := os.Getenv("TROUPE_HOME")
	if root == "" {
		root = ".troupe"
	}
	var taskStatus string
	flags := flag.NewFlagSet("troupe list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: troupe list [--status pending|running|blocked|done|failed] [--store <root>]")
		flags.PrintDefaults()
	}
	flags.StringVar(&taskStatus, "status", "", "filter by status: pending, running, blocked, done, failed (default all)")
	flags.StringVar(&root, "store", root, "store root (flag > TROUPE_HOME > .troupe)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	invalid := func(format string, args ...any) int {
		code := fail(stderr, format, args...)
		flags.Usage()
		return code
	}
	if flags.NArg() != 0 {
		return invalid("unexpected positional arguments")
	}
	statusSet := false
	flags.Visit(func(f *flag.Flag) { statusSet = statusSet || f.Name == "status" })
	if statusSet {
		switch store.Status(taskStatus) {
		case store.StatusPending, store.StatusRunning, store.StatusBlocked, store.StatusDone, store.StatusFailed:
		default:
			return invalid("invalid --status %q; choose pending, running, blocked, done, failed", taskStatus)
		}
	}
	if strings.TrimSpace(root) == "" {
		return invalid("--store must not be empty")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return fail(stderr, "resolve store root: %v", err)
	}
	fs, err := store.NewFileStore(root)
	if err != nil {
		return fail(stderr, "open store: %v", err)
	}
	tasks, err := fs.ListTasks(ctx, store.TaskFilter{Status: store.Status(taskStatus)})
	if err != nil {
		return fail(stderr, "list tasks: %v", err)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	for _, task := range tasks {
		// Keep goals on one line and truncate by rune to preserve Unicode text.
		goal := []rune(strings.Join(strings.Fields(task.Goal), " "))
		if len(goal) > 80 {
			goal = append(goal[:79], '…')
		}
		if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\n", task.ID, task.Status, string(goal)); err != nil {
			return fail(stderr, "write task list: %v", err)
		}
	}
	return 0
}

func runTask(ctx context.Context, task store.Task, agentName string, fs store.Store, root, webhookURL string, stdout, stderr io.Writer, agents map[string]agent.Agent, client *http.Client) int {
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
