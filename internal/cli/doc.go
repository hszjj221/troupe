// Package cli provides the troupe command-line interface using standard flags.
//
//	troupe run --agent codex --goal "Implement a bounded task" \
//	  --acceptance "go test ./..." --acceptance "go vet ./..."
//	troupe resume <id> --agent codex --note "Use the proposed approach."
//	troupe status <id> --store .troupe
//	troupe list --status blocked
//
// run requires --goal; positional goals are not accepted. --id defaults to a
// random t- prefixed ID; existing IDs are rejected. --role defaults to empty.
// --workdir defaults to the current directory, --context to empty, and repeatable
// --acceptance flags preserve each command as one string. --budget-minutes
// defaults to 30 and sets the agent timeout as well as task.Budget; it must be
// positive.
//
// resume requires exactly one task ID. Flags may appear before or after the ID.
// Only blocked tasks can resume; other statuses are rejected without changing
// the task. Optional --note appends a labeled user reply to Context, separated
// from existing context by a blank line, and saves it while still blocked.
// Optional --role replaces the stored task role, including when explicitly
// empty, and is also saved before running. The task then transitions blocked to
// pending before one more orchestrator attempt. The timeout uses the loaded
// task's Budget.MaxMinutes, which must be positive and fit in a time.Duration.
//
// run and resume select an agent by --agent, then the config binding for the
// task role, then default_agent; no match is a usage error. --agent always wins
// while --role still sets task metadata. resume uses the stored role unless
// --role is passed; tasks do not store the last agent. Only codex and claude
// are supported. Config agents.<name>.binary overrides their PATH defaults.
// YAML config lookup is --config, TROUPE_CONFIG, ./troupe.yaml, then config.yaml
// under TROUPE_HOME or .troupe. One file is loaded without merging; only missing
// implicit paths are ignored. Config lookup is independent of --store/--workdir.
//
// status requires exactly one task ID and prints ID, Status, Goal, Role, and
// Workdir as labeled lines. Flags may appear before or after the ID. Any stored
// status can be inspected; finding a task exits 0, including blocked/failed tasks.
// A missing task or any store or output error exits 1 with an error on stderr.
//
// list prints one tab-separated ID, status, and short goal per task, sorted by
// ID ascending, without a header. Goal whitespace is collapsed to single spaces;
// goals longer than 80 Unicode characters are truncated to 79 followed by an
// ellipsis. Optional --status accepts pending, running, blocked, done, or failed;
// omitting it lists all tasks. A successful list exits 0, with empty stdout when
// no tasks match. Invalid usage, store errors, and output errors exit 1.
// status and list only read tasks; they do not run agents or send webhooks.
//
// Store precedence is --store, then nonempty TROUPE_HOME, then .troupe under the
// current directory for all commands; an empty --store is a usage error (exit 1).
// Relative store and workdir paths resolve from that directory.
// New tasks are saved pending before one orchestrator attempt; runs live under
// <store-root>/runs/<task-id>/<round>. Output includes the agent result's ID,
// status, summary, commit, and notes, including any partial result on error.
//
// For both run and resume, a successful orchestrator return with a blocked result
// also triggers one JSON webhook POST. URL precedence is --webhook-url >
// TROUPE_WEBHOOK_URL > empty (disabled); --webhook-url= disables an environment
// URL. Done/failed results and orchestrator errors do not notify. Notification
// errors are printed to stderr and leave the blocked exit code at 1. No retries
// or redirects are attempted.
//
// For run and resume, exit 0 means a done result without errors. Exit 1 means a
// failed or blocked result, invalid usage, or any execution, persistence, or
// output error. Blocked reasons are printed in notes. Help exits 0 for all commands.
package cli
