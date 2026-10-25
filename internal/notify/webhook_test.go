package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/hszjj221/troupe/internal/store"
)

func TestNotifyBlocked(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls int
			var method, uri, contentType string
			var body []byte
			var bodyErr error
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				method, uri, contentType = r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type")
				body, bodyErr = io.ReadAll(r.Body)
				w.WriteHeader(status)
			}))
			defer server.Close()
			n := Notifier{URL: server.URL + "/hooks/blocked?source=troupe", Client: server.Client()}
			task := store.Task{ID: "task-1", Goal: "Finish the task"}
			result := store.Result{ID: task.ID, Status: store.ResultStatusBlocked, Summary: "Need input", Notes: "Which \"branch\"?\n请确认"}
			if err := n.NotifyBlocked(context.Background(), task, result); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || method != http.MethodPost || uri != "/hooks/blocked?source=troupe" || contentType != "application/json" {
				t.Fatalf("calls = %d, method = %q, URI = %q, Content-Type = %q", calls, method, uri, contentType)
			}
			if bodyErr != nil {
				t.Fatal(bodyErr)
			}
			var got map[string]string
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{
				"task_id": "task-1", "status": "blocked", "summary": "Need input",
				"notes": "Which \"branch\"?\n请确认", "goal": "Finish the task",
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("payload = %#v; want %#v", got, want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEmptyURL(t *testing.T) {
	n := Notifier{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("empty URL made a request")
		return nil, nil
	})}}
	if err := n.NotifyBlocked(context.Background(), store.Task{}, store.Result{}); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPError(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusServiceUnavailable, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
				io.WriteString(w, "hook unavailable")
			}))
			defer server.Close()
			client := server.Client()
			redirectCalls := 0
			client.CheckRedirect = func(*http.Request, []*http.Request) error {
				redirectCalls++
				return http.ErrUseLastResponse
			}
			n := Notifier{URL: server.URL, Client: client}
			err := n.NotifyBlocked(context.Background(), store.Task{}, store.Result{})
			if err == nil || !strings.Contains(err.Error(), http.StatusText(status)) || !strings.Contains(err.Error(), "hook unavailable") {
				t.Fatalf("error = %v; want response status and body", err)
			}
			if calls != 1 || redirectCalls != 0 {
				t.Fatalf("requests = %d, client redirect calls = %d; want 1, 0", calls, redirectCalls)
			}
			// The notifier must not replace the caller's redirect policy.
			client.CheckRedirect(nil, nil)
			if redirectCalls != 1 {
				t.Fatal("notifier changed the caller's client")
			}
		})
	}
}

func TestClientError(t *testing.T) {
	wantErr := errors.New("network unavailable")
	calls := 0
	n := Notifier{URL: "https://notify.example/blocked", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, wantErr
	})}}
	err := n.NotifyBlocked(context.Background(), store.Task{}, store.Result{})
	if calls != 1 || !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "post blocked webhook") {
		t.Fatalf("requests = %d, error = %v; want 1 and wrapped client error", calls, err)
	}
}

func TestInvalidURL(t *testing.T) {
	n := Notifier{URL: "://bad-url"}
	if err := n.NotifyBlocked(context.Background(), store.Task{}, store.Result{}); err == nil || !strings.Contains(err.Error(), "create blocked webhook request") {
		t.Fatalf("error = %v; want invalid URL error", err)
	}
}
