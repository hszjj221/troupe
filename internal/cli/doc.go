// Package cli provides the troupe command-line interface using standard flags.
//
//	troupe run --agent codex --goal "Implement a bounded task" \
//	  --acceptance "go test ./..." --acceptance "go vet ./..."
//	troupe resume <id> --agent codex --note "Use the proposed approach."
//
// run requires --goal and --agent (codex or claude); positional goals are not
// accepted. --id defaults to a random t- prefixed ID; existing IDs are rejected.
// --role defaults to empty and does not select the agent. --workdir defaults to
// the current directory, --context to empty, and repeatable --acceptance flags
// preserve each command as one string. --budget-minutes defaults to 30 and sets
// the agent timeout as well as task.Budget; it must be positive.
//
// resume requires exactly one task ID and --agent (codex or claude). The agent
// must be re-specified because tasks do not store the last agent. Flags may
// appear before or after the ID. Only blocked tasks can resume; other statuses
// are rejected without changing the task. Optional --note appends a labeled
// user reply to Context, separated from existing context by a blank line, and
// saves it while still blocked. The task then transitions blocked to pending
// before one more orchestrator attempt. The timeout uses the loaded task's
// Budget.MaxMinutes, which must be positive and fit in a time.Duration.
//
// Store precedence is --store, then nonempty TROUPE_HOME, then .troupe under the
// current directory. Relative store and workdir paths resolve from that directory.
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
// Exit 0 means a done result without errors, or help. Exit 1 means a failed or
// blocked result, invalid usage, or any execution, persistence, or output error.
// Blocked reasons are printed in notes.
package cli
