package api

import (
	"testing"
	"time"

	"github.com/iQonAi/devbox/internal/store"
)

func TestSummarize(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	ev := func(sec int, typ, msg string) store.Event {
		return store.Event{TS: t0.Add(time.Duration(sec) * time.Second), Type: typ, Message: msg}
	}
	task := store.Task{ID: "t1", Source: "task", State: store.StateFailed, CreatedAt: t0, Ref: "tab-split.p1.split.a1"}

	got := Summarize(task, []store.Event{
		ev(0, store.EventState, "Created"),
		ev(1, store.EventState, "Created->Running"),
		ev(2, store.EventPhase, "run agent"),
		ev(60, store.EventState, "->Failed: budget_exceeded: the agent's budget stop fired"),
	})
	want := TaskStatus{ID: "t1", State: "Failed", Source: "task", Ref: "tab-split.p1.split.a1", CreatedAt: "2026-10-15T12:00:00Z", LastEventAt: "2026-10-15T12:01:00Z", Error: "budget_exceeded: the agent's budget stop fired"}
	if got != want {
		t.Errorf("failed task:\n got %+v\nwant %+v", got, want)
	}

	// A completed task carries no error even though an earlier transition had a reason; the change URL rides along.
	task.State, task.PRURL = store.StateCompleted, "https://gitlab.com/g/p/-/merge_requests/4"
	got = Summarize(task, []store.Event{ev(0, store.EventState, "Created->Running"), ev(5, store.EventState, "->Completed")})
	if got.Error != "" || got.PRURL != task.PRURL || got.State != "Completed" || got.LastEventAt != "2026-10-15T12:00:05Z" {
		t.Errorf("completed task: %+v", got)
	}

	// Cancelled before start: the reason is on the Created->Cancelled event.
	task.State, task.PRURL = store.StateCancelled, ""
	got = Summarize(task, []store.Event{ev(0, store.EventState, "Created"), ev(1, store.EventState, "Created->Cancelled: cancelled before start")})
	if got.Error != "cancelled before start" {
		t.Errorf("cancelled task error = %q", got.Error)
	}

	// No events at all (a task the pool never reached): no last event, no error.
	got = Summarize(store.Task{ID: "t2", State: store.StateCreated, CreatedAt: t0}, nil)
	if got.LastEventAt != "" || got.Error != "" || got.Ref != "" {
		t.Errorf("bare task: %+v", got)
	}
}
