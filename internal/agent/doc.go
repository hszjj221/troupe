// Package agent defines the Agent interface using store.Task and store.Result.
// Its Codex and Claude stubs shell out to codex exec and claude -p with task.yaml
// and result.json as the file protocol, and save stdout and stderr under the run
// directory's logs/.
package agent
