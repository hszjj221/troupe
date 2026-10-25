package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hszjj221/troupe/internal/store"
)

// Notifier sends blocked task notifications to URL. An empty URL disables it.
// Client may supply a custom transport; nil uses a client with a 10-second timeout.
type Notifier struct {
	URL    string
	Client *http.Client
}

// NotifyBlocked posts one JSON notification without retries or redirects.
// Callers invoke it only after a successful run returns a blocked result.
func (n Notifier) NotifyBlocked(ctx context.Context, task store.Task, result store.Result) error {
	if n.URL == "" {
		return nil
	}
	payload := struct {
		TaskID  string             `json:"task_id"`
		Status  store.ResultStatus `json:"status"`
		Summary string             `json:"summary"`
		Notes   string             `json:"notes"`
		Goal    string             `json:"goal"`
	}{task.ID, store.ResultStatusBlocked, result.Summary, result.Notes, task.Goal}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode blocked webhook: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create blocked webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := http.Client{Timeout: 10 * time.Second}
	if n.Client != nil {
		client = *n.Client
	}
	// Copy the client so suppressing redirects does not change the caller's client.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post blocked webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if err != nil {
			return fmt.Errorf("blocked webhook returned %s; read response body: %w", resp.Status, err)
		}
		return fmt.Errorf("blocked webhook returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
