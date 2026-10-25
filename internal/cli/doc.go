// Package cli provides the troupe command-line interface using standard flags.
//
//	troupe run --agent codex --goal "Implement a bounded task" \
//	  --acceptance "go test ./..." --acceptance "go vet ./..."
//
// run requires --goal and --agent (codex or claude); positional goals are not
// accepted. --id defaults to a random t- prefixed ID; existing IDs are rejected.
// --role defaults to empty and does not select the agent. --workdir defaults to
// the current directory, --context to empty, and repeatable --acceptance flags
// preserve each command as one string. --budget-minutes defaults to 30 and sets
// the agent timeout as well as task.Budget; it must be positive.
//
// Store precedence is --store, then nonempty TROUPE_HOME, then .troupe under the
// current directory. Relative store and workdir paths resolve from that directory.
// Tasks are saved pending before one orchestrator attempt; runs live under
// <store-root>/runs/<task-id>/<round>. Output includes the agent result's ID,
// status, summary, commit, and notes, including any partial result on error.
//
// A successful orchestrator return with a blocked result also triggers one JSON
// webhook POST. URL precedence is --webhook-url > TROUPE_WEBHOOK_URL > empty
// (disabled); --webhook-url= disables an environment URL. Done/failed results and
// orchestrator errors do not notify. Notification errors are printed to stderr
// and leave the blocked exit code at 1. No retries or redirects are attempted.
//
// Exit 0 means a done result without errors, or help. Exit 1 means a failed or
// blocked result, invalid usage, or any execution, persistence, or output error.
// Blocked reasons are printed in notes; resume is not provided.
package cli
