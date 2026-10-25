package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

var (
	ErrNotFound       = errors.New("task not found")
	ErrStatusConflict = errors.New("task status conflict")
)

// FileStore is safe for concurrent use. A root must be owned by one FileStore;
// separate instances or processes require external coordination.
type FileStore struct {
	root string
	mu   sync.Mutex
}

var _ Store = (*FileStore)(nil)

func NewFileStore(root string) (*FileStore, error) {
	for _, dir := range []string{"tasks", "runs"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return nil, fmt.Errorf("create %s directory: %w", dir, err)
		}
	}
	return &FileStore{root: root}, nil
}

func (s *FileStore) SaveTask(ctx context.Context, t Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateID(t.ID); err != nil {
		return err
	}
	return s.writeTask(t)
}

func (s *FileStore) GetTask(ctx context.Context, id string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	if err := validateID(id); err != nil {
		return Task{}, err
	}
	return s.readTask(id)
}

func (s *FileStore) ListTasks(ctx context.Context, filter TaskFilter) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "tasks"))
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	tasks := []Task{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t, err := s.readTask(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if filter.Status == "" || t.Status == filter.Status {
			tasks = append(tasks, t)
		}
	}
	return tasks, nil
}

func (s *FileStore) UpdateStatus(ctx context.Context, id string, from, to Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateID(id); err != nil {
		return err
	}
	t, err := s.readTask(id)
	if err != nil {
		return err
	}
	if t.Status != from {
		return fmt.Errorf("%w: task %q is %q, expected %q", ErrStatusConflict, id, t.Status, from)
	}
	t.Status = to
	return s.writeTask(t)
}

func (s *FileStore) SaveRun(ctx context.Context, r Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateID(r.TaskID); err != nil {
		return err
	}
	if r.N < 1 || r.Task.ID != r.TaskID || r.Result.ID != r.TaskID {
		return fmt.Errorf("invalid run: round must be positive and payload IDs must match task %q", r.TaskID)
	}
	taskData, err := yaml.Marshal(r.Task)
	if err != nil {
		return fmt.Errorf("encode run task: %w", err)
	}
	resultData, err := json.MarshalIndent(r.Result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode run result: %w", err)
	}
	dir := filepath.Join(s.root, "runs", r.TaskID, strconv.Itoa(r.N))
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		return fmt.Errorf("create run directory: %w", err)
	}
	if err := atomicWrite(filepath.Join(dir, "task.yaml"), taskData); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "result.json"), resultData)
}

func (s *FileStore) ListRuns(ctx context.Context, taskID string) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateID(taskID); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, "runs", taskID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Run{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list runs for %q: %w", taskID, err)
	}
	runs := []Run{}
	for _, entry := range entries {
		n, err := strconv.Atoi(entry.Name())
		if !entry.IsDir() || err != nil || n < 1 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r := Run{TaskID: taskID, N: n}
		runDir := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(filepath.Join(runDir, "task.yaml"))
		if err != nil {
			return nil, fmt.Errorf("read run %q/%d task: %w", taskID, n, err)
		}
		if err := yaml.Unmarshal(data, &r.Task); err != nil {
			return nil, fmt.Errorf("decode run %q/%d task: %w", taskID, n, err)
		}
		data, err = os.ReadFile(filepath.Join(runDir, "result.json"))
		if err != nil {
			return nil, fmt.Errorf("read run %q/%d result: %w", taskID, n, err)
		}
		if err := json.Unmarshal(data, &r.Result); err != nil {
			return nil, fmt.Errorf("decode run %q/%d result: %w", taskID, n, err)
		}
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].N < runs[j].N })
	return runs, nil
}

func (s *FileStore) readTask(id string) (Task, error) {
	data, err := os.ReadFile(filepath.Join(s.root, "tasks", id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return Task{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if err != nil {
		return Task{}, fmt.Errorf("read task %q: %w", id, err)
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return Task{}, fmt.Errorf("decode task %q: %w", id, err)
	}
	return t, nil
}

func (s *FileStore) writeTask(t Task) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("encode task %q: %w", t.ID, err)
	}
	return atomicWrite(filepath.Join(s.root, "tasks", t.ID+".json"), data)
}

func validateID(id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\\x00") {
		return fmt.Errorf("invalid task ID %q: must be a single path component", id)
	}
	return nil
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file for %q: %w", path, err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write temporary file for %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temporary file for %q: %w", path, err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("replace %q: %w", path, err)
	}
	return nil
}
