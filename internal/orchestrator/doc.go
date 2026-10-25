// Package orchestrator runs one persisted pending task with an explicitly named
// agent. Callers save tasks first and supply a run root; for FileStore, use
// <store-root>/runs so agent logs and saved runs share <task-id>/<round>.
//
// Each Orchestrator serializes its calls. It neither retries nor switches agents.
// Results are archived before the final status update. Errors after an agent
// attempt mark the task failed; partial results are retained, with an absent ID
// filled from the task. An empty result becomes a failed result with error notes.
// Final persistence ignores cancellation so an interrupted agent can be recorded.
// Store errors are returned, and a failed status update may leave a task running.
// Role bindings, acceptance checks, CLI, and notifications are left to callers.
package orchestrator
