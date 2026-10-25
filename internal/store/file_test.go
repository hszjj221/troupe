package store

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func sampleTask(id string, status Status) Task {
	return Task{
		ID:         id,
		Goal:       "增加环境变量覆盖，并补充单元测试",
		Role:       "coder",
		Workdir:    "/path/to/repo",
		Acceptance: []string{"go build ./...", "go vet ./...", "go test ./..."},
		Context:    "配置加载代码在 internal/config 下。\nKeep existing defaults.\n",
		Budget:     Budget{MaxMinutes: 30},
		Status:     status,
	}
}

func openStore(t *testing.T, root string) *FileStore {
	t.Helper()
	s, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func assertNoTempFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("temporary file left behind: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSaveGetTask(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := openStore(t, root)
	want := sampleTask("task-1", StatusPending)
	if err := s.SaveTask(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := openStore(t, root).GetTask(ctx, want.ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("GetTask = %#v, %v; want %#v", got, err, want)
	}
	path := filepath.Join(root, "tasks", want.ID+".json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	want.Goal = "shorter"
	want.Status = StatusDone
	if err := s.SaveTask(ctx, want); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("SaveTask reused the original file instead of renaming a replacement")
	}
	got, err = s.GetTask(ctx, want.ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("GetTask after overwrite = %#v, %v; want %#v", got, err, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "goal", "role", "workdir", "acceptance", "context", "budget", "status"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("task JSON missing %q", key)
		}
	}
	var budget map[string]int
	if err := json.Unmarshal(fields["budget"], &budget); err != nil || budget["max_minutes"] != 30 {
		t.Fatalf("budget JSON = %s, %v", fields["budget"], err)
	}
	assertNoTempFiles(t, root)
}

func TestListTasks(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	all := []Task{sampleTask("a", StatusPending), sampleTask("b", StatusDone), sampleTask("c", StatusPending)}
	for _, task := range all {
		if err := s.SaveTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name   string
		filter TaskFilter
		want   []Task
	}{
		{"all", TaskFilter{}, all},
		{"pending", TaskFilter{Status: StatusPending}, []Task{all[0], all[2]}},
		{"no matches", TaskFilter{Status: StatusBlocked}, []Task{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ListTasks(ctx, tt.filter)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ListTasks = %#v, %v; want %#v", got, err, tt.want)
			}
		})
	}
}

func TestUpdateStatus(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := openStore(t, root)
	want := sampleTask("task-1", StatusPending)
	if err := s.SaveTask(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, want.ID, StatusPending, StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, want.ID, StatusPending, StatusDone); !errors.Is(err, ErrStatusConflict) {
		t.Fatalf("wrong from status: got %v, want ErrStatusConflict", err)
	}
	want.Status = StatusRunning
	got, err := s.GetTask(ctx, want.ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("GetTask after CAS = %#v, %v; want %#v", got, err, want)
	}
	if _, err := s.GetTask(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTask missing = %v, want ErrNotFound", err)
	}
	if err := s.UpdateStatus(ctx, "missing", StatusPending, StatusRunning); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateStatus missing = %v, want ErrNotFound", err)
	}
	assertNoTempFiles(t, root)
}

func TestUpdateStatusConcurrent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	if err := s.SaveTask(ctx, sampleTask("task-1", StatusPending)); err != nil {
		t.Fatal(err)
	}
	const writers = 16
	start := make(chan struct{})
	results := make(chan error, writers)
	for i := 0; i < writers; i++ {
		go func() {
			<-start
			results <- s.UpdateStatus(ctx, "task-1", StatusPending, StatusRunning)
		}()
	}
	close(start)
	successes := 0
	for i := 0; i < writers; i++ {
		if err := <-results; err == nil {
			successes++
		} else if !errors.Is(err, ErrStatusConflict) {
			t.Errorf("UpdateStatus = %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful CAS calls = %d, want 1", successes)
	}
}

func TestSaveListRuns(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := openStore(t, root)
	if runs, err := s.ListRuns(ctx, "task-1"); err != nil || len(runs) != 0 {
		t.Fatalf("ListRuns before SaveRun = %#v, %v", runs, err)
	}
	want := []Run{}
	for i, n := range []int{1, 2, 10} {
		want = append(want, Run{
			TaskID: "task-1", N: n,
			Task: sampleTask("task-1", StatusRunning),
			Result: Result{
				ID: "task-1", Status: []ResultStatus{ResultStatusBlocked, ResultStatusFailed, ResultStatusDone}[i],
				Summary: "round " + strconv.Itoa(n), Commit: "a1b2c3d", Notes: "some notes\nnext line",
			},
		})
	}
	for _, run := range []Run{want[2], want[0], want[1]} {
		if err := s.SaveRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, "runs", run.TaskID, strconv.Itoa(run.N))
		info, err := os.Stat(filepath.Join(dir, "logs"))
		if err != nil || !info.IsDir() {
			t.Fatalf("logs directory missing: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "task.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := yaml.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if json.Valid(data) || fields["id"] != run.TaskID || !strings.Contains(string(data), "max_minutes: 30") {
			t.Fatalf("unexpected task YAML: %s", data)
		}
	}
	got, err := openStore(t, root).ListRuns(ctx, "task-1")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ListRuns = %#v, %v; want %#v", got, err, want)
	}
	assertNoTempFiles(t, root)
}

func TestInvalidIDsAndRuns(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	for _, id := range []string{"", ".", "..", "../escape", "a/b", `a\b`, "a\x00b"} {
		if err := s.SaveTask(ctx, sampleTask(id, StatusPending)); err == nil {
			t.Errorf("SaveTask accepted invalid ID %q", id)
		}
	}
	for _, run := range []Run{
		{TaskID: "../escape", N: 1},
		{TaskID: "task-1", N: 0, Task: sampleTask("task-1", StatusRunning), Result: Result{ID: "task-1"}},
		{TaskID: "task-1", N: 1, Task: sampleTask("other", StatusRunning), Result: Result{ID: "task-1"}},
		{TaskID: "task-1", N: 1, Task: sampleTask("task-1", StatusRunning), Result: Result{ID: "other"}},
	} {
		if err := s.SaveRun(ctx, run); err == nil {
			t.Errorf("SaveRun accepted invalid run %#v", run)
		}
	}
}
