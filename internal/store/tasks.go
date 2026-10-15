package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Task is a row in the tasks table, as the API and CLI show it: the identity
// and lifecycle columns, plus the client reference and the change URL once set.
type Task struct {
	ID        string    `json:"id"`
	RepoID    int64     `json:"repo_id"`
	Source    string    `json:"source"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	// Ref is the submitter's reference for this task (submit --ref); "" when none.
	// Unique among tasks that carry one: a repeat submit with the same ref is the same task.
	Ref string `json:"ref,omitempty"`
	// PRURL is the change opened for the task (a pull or merge request); "" until one is.
	PRURL string `json:"pr_url,omitempty"`
}

// ErrDuplicateRef is returned by CreateTask when another task already carries
// the reference; the caller looks that task up with TaskByRef.
var ErrDuplicateRef = errors.New("a task with this ref already exists")

// Task states, stored verbatim as in the design ("0002", verbatim)
// Created and Running are live; the rest are terminal.
const (
	StateCreated   = "Created"
	StateRunning   = "Running"
	StateCompleted = "Completed"
	StateFailed    = "Failed"
	StateCancelled = "Cancelled"
)

// NewTask is the input for creating a task, Nullable lifecycle columns
// (exit_code, pr_url, ...) are filled in later as the task progresses.
type NewTask struct {
	ID           string
	RepoID       int64
	Source       string
	Agent        string
	Branch       string
	HostWorktree string
	Ref          string // optional client reference; stored NULL when ""
}

// CreateTask inserts a task in the Created state. A ref another task already
// carries is ErrDuplicateRef (the partial unique index enforces it, so two
// concurrent submits with one ref cannot both insert).
func (s *Store) CreateTask(t NewTask) error {
	var ref sql.NullString
	if t.Ref != "" {
		ref = sql.NullString{String: t.Ref, Valid: true}
	}
	_, err := s.db.Exec(
		`
			INSERT INTO tasks (id, repo_id, source, agent, branch, host_worktree, state, created_at, ref)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.RepoID, t.Source, t.Agent, t.Branch, t.HostWorktree, StateCreated, time.Now().UTC(), ref,
	)
	if err != nil {
		// modernc sqlite words it "UNIQUE constraint failed: tasks.ref"; the index name is not in the message.
		if t.Ref != "" && strings.Contains(err.Error(), "UNIQUE constraint failed: tasks.ref") {
			return fmt.Errorf("create task %q with ref %q: %w", t.ID, t.Ref, ErrDuplicateRef)
		}
		return fmt.Errorf("create task %q: %w", t.ID, err)
	}
	return nil
}

// TaskByRef returns the task carrying ref, or nil when none does.
func (s *Store) TaskByRef(ref string) (*Task, error) {
	if ref == "" {
		return nil, nil
	}
	row := s.db.QueryRow(`SELECT id, repo_id, source, state, created_at, COALESCE(ref, ''), COALESCE(pr_url, '') FROM tasks WHERE ref = ?`, ref)
	var t Task
	if err := row.Scan(&t.ID, &t.RepoID, &t.Source, &t.State, &t.CreatedAt, &t.Ref, &t.PRURL); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("task by ref %q: %w", ref, err)
	}
	return &t, nil
}

// UpdateTaskState moves a task to a new state
func (s *Store) UpdateTaskState(id, state string) error {
	res, err := s.db.Exec(`UPDATE tasks SET state = ? WHERE id = ?`, state, id)
	if err != nil {
		return fmt.Errorf("update task %q state: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no task with id %q", id)
	}
	return nil
}

// SetTaskBranchWorktree records a task's feature branch and host worktree path
// as soon as they exist, so the orphan sweep can reclaim the worktree once the
// task reaches a terminal state.
func (s *Store) SetTaskBranchWorktree(id, branch, worktree string) error {
	res, err := s.db.Exec(`UPDATE tasks SET branch = ?, host_worktree = ? WHERE id = ?`, branch, worktree, id)
	if err != nil {
		return fmt.Errorf("set branch/worktree for %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no task with id %q", id)
	}
	return nil
}

// ListTasks returns all tasks, newest first.
func (s *Store) ListTasks() ([]Task, error) {
	rows, err := s.db.Query(`
					SELECT id, repo_id, source, state, created_at, COALESCE(ref, ''), COALESCE(pr_url, '')
					FROM tasks
					ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.RepoID, &t.Source, &t.State, &t.CreatedAt, &t.Ref, &t.PRURL); err != nil {
			return nil, fmt.Errorf("scan tasks: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return tasks, nil
}

// SweepableTask is a terminal-state task whose host worktree may still exist on
// disk - the orphan sweep's work list.
type SweepableTask struct {
	ID           string
	Branch       string
	HostWorktree string
	MirrorPath   string
}

// SweepableTasks returns terminal-state tasks that still carry a worktree path,
// joined to their repo's mirror so the sweeper can act on them.
func (s *Store) SweepableTasks() ([]SweepableTask, error) {
	rows, err := s.db.Query(
		`
		  SELECT t.id, t.branch, t.host_worktree, r.mirror_path
              FROM tasks t
              JOIN repos r ON r.id = t.repo_id
              WHERE t.state IN (?, ?, ?)
                AND t.host_worktree IS NOT NULL
                AND t.host_worktree != ''`,
		StateCompleted, StateFailed, StateCancelled,
	)
	if err != nil {
		return nil, fmt.Errorf("query sweepable tasks: %w", err)
	}
	defer rows.Close()

	var tasks []SweepableTask
	for rows.Next() {
		var t SweepableTask

		// branch and mirror_path are nullable columns; a plain string cannot
		// hold NULL, so scan into sql.NullString and read the .String field
		// (which is "" when the column was NULL).
		var branch, mirror sql.NullString
		if err := rows.Scan(&t.ID, &branch, &t.HostWorktree, &mirror); err != nil {
			return nil, fmt.Errorf("scan sweepable task: %w", err)
		}
		t.Branch = branch.String
		t.MirrorPath = mirror.String
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sweepable tasks: %w", err)
	}
	return tasks, nil
}

// ClearTaskWorktree forgets a task's worktree path once the worktree is gone,
// so the startup sweep does not process it again. Idempotent by design: a
// no-op on an unknown id is fine, so now RowsAffected check.
func (s *Store) ClearTaskWorktree(id string) error {
	if _, err := s.db.Exec(`UPDATE tasks SET host_worktree = NULL WHERE id = ?`, id); err != nil {
		return fmt.Errorf("clear worktree for %q: %w", id, err)
	}
	return nil
}

// SetTaskPRURL records the pull request opened for a task (M4 back-link).
func (s *Store) SetTaskPRURL(id, url string) error {
	res, err := s.db.Exec(`UPDATE tasks SET pr_url = ? WHERE id = ?`, url, id)
	if err != nil {
		return fmt.Errorf("set pr_url for %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no task with id %q", id)
	}
	return nil
}
