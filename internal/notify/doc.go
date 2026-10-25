// Package notify posts blocked task notifications as JSON webhooks. The payload
// includes task_id, status (blocked), summary, notes, and goal. An empty URL is a
// no-op; otherwise each call makes one POST, without retries or redirects.
// A nil Client uses a 10-second timeout; a supplied http.Client permits a custom
// timeout or RoundTripper. Request contexts control cancellation.
//
// The CLI calls NotifyBlocked only after a successful orchestrator return with a
// blocked result. URL precedence is --webhook-url > TROUPE_WEBHOOK_URL > empty
// (disabled); an explicitly empty flag disables an environment URL. Non-2xx
// responses return an error with the status and up to 1024 bytes of response body;
// client errors are wrapped. The CLI prints notification errors to stderr and
// keeps the blocked exit code of 1, along with the usual result output and notes.
package notify
