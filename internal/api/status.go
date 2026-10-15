package api

import (
	"strings"
	"time"

	"github.com/iQonAi/devbox/internal/store"
)

// TaskStatus is a task's state as one machine-readable line (agent-task status
// --json): what the plane's scheduler reads through the build host's command.
type TaskStatus struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Source      string `json:"source"`
	Ref         string `json:"ref,omitempty"`
	PRURL       string `json:"prUrl,omitempty"`
	CreatedAt   string `json:"createdAt"`
	LastEventAt string `json:"lastEventAt,omitempty"`
	// Error is why a Failed or Cancelled task ended so: the reason on its last
	// terminal state event ("->Failed: budget_exceeded: ..."), "" otherwise.
	Error string `json:"error,omitempty"`
}

// Summarize renders a task and its audit trail as a TaskStatus.
func Summarize(task store.Task, events []store.Event) TaskStatus {
	ts := TaskStatus{ID: task.ID, State: task.State, Source: task.Source, Ref: task.Ref, PRURL: task.PRURL, CreatedAt: task.CreatedAt.UTC().Format(time.RFC3339)}
	for _, e := range events {
		ts.LastEventAt = e.TS.UTC().Format(time.RFC3339)
		if e.Type != store.EventState {
			continue
		}
		// "->Failed: reason", "Created->Cancelled: reason"; a transition without a reason clears it.
		if i := strings.Index(e.Message, ": "); i >= 0 && strings.Contains(e.Message[:i], "->") {
			ts.Error = e.Message[i+2:]
		} else {
			ts.Error = ""
		}
	}
	if task.State != store.StateFailed && task.State != store.StateCancelled {
		ts.Error = ""
	}
	return ts
}
