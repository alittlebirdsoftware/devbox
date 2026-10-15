package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iQonAi/devbox/internal/api"
	"github.com/iQonAi/devbox/internal/config"
	"github.com/iQonAi/devbox/internal/controller"
	"github.com/iQonAi/devbox/internal/pool"
	"github.com/iQonAi/devbox/internal/store"
)

func TestSubmitCreatesTaskAndEnqueues(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "") // no secrets in the test

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	got := make(chan controller.Request, 1)
	p := pool.New(context.Background(), func(_ context.Context, req controller.Request) (controller.Outcome, error) {
		got <- req
		return controller.Outcome{}, nil
	}, 1, 4)

	cfg := &config.Config{
		DataDir: t.TempDir(),
		Limits:  config.Limits{MaxConcurrent: 1, TaskTimeout: "30m"},
		Repos:   []config.Repo{{Name: "devbox", Owner: "iQonAi", Repo: "devbox", DefaultBranch: "main", TokenRef: "gh"}},
		Agents:  map[string]config.AgentConfig{"mock": {Auth: "api_key", TokenRef: "model"}},
	}
	s := &submitter{cfg: cfg, store: st, pool: p}

	res, err := s.Submit(api.SubmitRequest{Repo: "devbox", Agent: "mock", Task: "do a thing"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	id := res.TaskID
	if res.Existing {
		t.Error("a first submit reported an existing task")
	}

	req := <-got
	if req.TaskID != id || req.RepoName != "devbox" || req.Title != "do a thing" {
		t.Errorf("enqueued req = %+v", req)
	}
	tasks, _ := st.ListTasks()
	if len(tasks) != 1 || tasks[0].ID != id {
		t.Fatalf("task not persisted: %+v", tasks)
	}

	if _, err := s.Submit(api.SubmitRequest{Repo: "nope", Agent: "mock", Task: "x"}); err == nil {
		t.Error("expected error for unknown repo")
	}
	if _, err := s.Submit(api.SubmitRequest{Repo: "devbox", Agent: "ghost", Task: "x"}); err == nil {
		t.Error("expected error for unconfigured agent")
	}
}

// fakeKiller records which tasks had their containers destroyed and can
// simulate a podman failure.
type fakeKiller struct {
	destroyed []string
	err       error
}

func (f *fakeKiller) Destroy(_ context.Context, taskID string) error {
	f.destroyed = append(f.destroyed, taskID)
	return f.err
}

func TestRecoverInflight(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.UpsertRepo(store.Repo{Name: "r", Owner: "o", Repo: "r", DefaultBranch: "main", TokenRef: "t"}); err != nil {
		t.Fatal(err)
	}
	repos, _ := st.ListRepos()
	id := repos[0].ID

	mk := func(tid, state string) {
		if err := st.CreateTask(store.NewTask{ID: tid, RepoID: id, Source: "task"}); err != nil {
			t.Fatal(err)
		}
		if state != store.StateCreated {
			_ = st.UpdateTaskState(tid, state)
		}
	}
	mk("running", store.StateRunning)
	mk("created", store.StateCreated)
	mk("done", store.StateCompleted)

	killer := &fakeKiller{}
	if n := recoverInflight(context.Background(), st, killer); n != 2 {
		t.Errorf("recovered %d, want 2 (running + created)", n)
	}
	tasks, _ := st.ListTasks()
	for _, task := range tasks {
		if task.ID == "done" && task.State != store.StateCompleted {
			t.Errorf("terminal task was touched: %s", task.State)
		}
		if (task.ID == "running" || task.ID == "created") && task.State != store.StateFailed {
			t.Errorf("%s not failed: %s", task.ID, task.State)
		}
	}

	// Orphaned containers are destroyed for both recovered tasks — and only
	// those — and the destruction is audited.
	if len(killer.destroyed) != 2 {
		t.Fatalf("destroyed %v, want the 2 recovered tasks", killer.destroyed)
	}
	for _, tid := range killer.destroyed {
		if tid != "running" && tid != "created" {
			t.Errorf("destroyed container for non-recovered task %q", tid)
		}
		events, _ := st.ListEvents(tid)
		found := false
		for _, e := range events {
			if e.Type == store.EventSecurity && e.Message == "container destroyed (recovery)" {
				found = true
			}
		}
		if !found {
			t.Errorf("no destroy event for %q: %+v", tid, events)
		}
	}
}

// A podman failure during recovery is best-effort: still recovered, no
// destroy event claimed.
func TestRecoverInflightDestroyFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.UpsertRepo(store.Repo{Name: "r", Owner: "o", Repo: "r", DefaultBranch: "main", TokenRef: "t"}); err != nil {
		t.Fatal(err)
	}
	repos, _ := st.ListRepos()
	if err := st.CreateTask(store.NewTask{ID: "stuck", RepoID: repos[0].ID, Source: "task"}); err != nil {
		t.Fatal(err)
	}
	_ = st.UpdateTaskState("stuck", store.StateRunning)

	killer := &fakeKiller{err: fmt.Errorf("podman unavailable")}
	if n := recoverInflight(context.Background(), st, killer); n != 1 {
		t.Errorf("recovered %d, want 1", n)
	}
	tasks, _ := st.ListTasks()
	if tasks[0].State != store.StateFailed {
		t.Errorf("state = %q, want Failed", tasks[0].State)
	}
	events, _ := st.ListEvents("stuck")
	for _, e := range events {
		if e.Message == "container destroyed (recovery)" {
			t.Error("destroy event recorded although rm failed")
		}
	}
}

// Cancelling a task that is still queued records Created->Cancelled: nothing
// else will, since the pool never runs it.
func TestCancelQueuedRecordsCancelled(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// One worker, held busy by the first task so the second stays queued.
	started := make(chan string, 1)
	release := make(chan struct{})
	p := pool.New(context.Background(), func(_ context.Context, req controller.Request) (controller.Outcome, error) {
		started <- req.TaskID
		<-release
		return controller.Outcome{}, nil
	}, 1, 4)
	defer close(release)

	cfg := &config.Config{
		DataDir: t.TempDir(),
		Limits:  config.Limits{MaxConcurrent: 1, TaskTimeout: "30m"},
		Repos:   []config.Repo{{Name: "devbox", Owner: "iQonAi", Repo: "devbox", DefaultBranch: "main", TokenRef: "gh"}},
		Agents:  map[string]config.AgentConfig{"mock": {Auth: "api_key", TokenRef: "model"}},
	}
	s := &submitter{cfg: cfg, store: st, pool: p}

	if _, err := s.Submit(api.SubmitRequest{Repo: "devbox", Agent: "mock", Task: "hold the worker"}); err != nil {
		t.Fatalf("submit 1: %v", err)
	}
	<-started
	queued, err := s.Submit(api.SubmitRequest{Repo: "devbox", Agent: "mock", Task: "stay queued"})
	if err != nil {
		t.Fatalf("submit 2: %v", err)
	}
	queuedID := queued.TaskID

	if err := s.Cancel(queuedID); err != nil {
		t.Fatalf("cancel queued: %v", err)
	}

	tasks, _ := st.ListTasks()
	for _, task := range tasks {
		if task.ID == queuedID && task.State != store.StateCancelled {
			t.Errorf("queued task state = %q, want Cancelled", task.State)
		}
	}
	events, _ := st.ListEvents(queuedID)
	found := false
	for _, e := range events {
		if e.Type == store.EventState && e.Message == "Created->Cancelled: cancelled before start" {
			found = true
		}
	}
	if !found {
		t.Errorf("no Created->Cancelled event for %s: %+v", queuedID, events)
	}
}

func TestSubmitBuildFields(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	got := make(chan controller.Request, 4)
	p := pool.New(context.Background(), func(_ context.Context, req controller.Request) (controller.Outcome, error) {
		got <- req
		return controller.Outcome{}, nil
	}, 1, 4)
	cfg := &config.Config{
		DataDir: t.TempDir(), Image: "global",
		Limits: config.Limits{MaxConcurrent: 1, TaskTimeout: "30m"},
		Repos: []config.Repo{
			{Name: "app", Owner: "o", Repo: "app", DefaultBranch: "main", TokenRef: "gh",
				Image: "flutter", Limits: config.RepoLimits{MemoryMB: 4096},
				Check: "project-check", ResultContract: config.ResultContractV1},
			{Name: "hero", Owner: "o", Repo: "hero", DefaultBranch: "main", TokenRef: "gh"},
		},
		Agents: map[string]config.AgentConfig{
			"claude": {Auth: "subscription", TokenRef: "model"},
			"pi":     {Auth: "api_key", TokenRef: "model"},
		},
	}
	s := &submitter{cfg: cfg, store: st, pool: p}

	_, err = s.Submit(api.SubmitRequest{Repo: "app", Agent: "claude", Task: "w1",
		Scope: "ticket:w1", ActiveSet: []byte(`{"active":["f1.1.v1"]}`), BudgetUSD: 3, MaxTurns: 60})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	req := <-got
	if req.Image != "flutter" || req.Limits.MemoryMB != 4096 || req.Check != "project-check" ||
		req.ResultContract != "v1" || req.CheckScope != "ticket:w1" || req.ActiveSet != `{"active":["f1.1.v1"]}` ||
		req.Budget.MaxUSD != 3 || req.Budget.MaxTurns != 60 {
		t.Errorf("enqueued req = %+v", req)
	}

	refused := map[string]api.SubmitRequest{
		"check repo without scope":  {Repo: "app", Agent: "claude", Task: "x"},
		"scope on a repo w/o check": {Repo: "hero", Agent: "claude", Task: "x", Scope: "regression"},
		"unknown scope":             {Repo: "app", Agent: "claude", Task: "x", Scope: "everything"},
		"active set not an object":  {Repo: "app", Agent: "claude", Task: "x", Scope: "regression", ActiveSet: []byte(`["a"]`)},
		"scaffold with active":      {Repo: "app", Agent: "claude", Task: "x", Scope: "scaffold", ActiveSet: []byte(`{"active":["a"]}`)},
		"budget for pi":             {Repo: "hero", Agent: "pi", Task: "x", BudgetUSD: 1},
		"negative turns":            {Repo: "hero", Agent: "claude", Task: "x", MaxTurns: -1},
	}
	for name, sr := range refused {
		if _, err := s.Submit(sr); err == nil {
			t.Errorf("%s: submit accepted", name)
		}
	}
}

// A submit carrying a ref is idempotent: the second answer is the first task,
// marked existing, with nothing new filed or enqueued — whatever state the
// first task reached. A bad ref is refused before anything is read.
func TestSubmitRefIsIdempotent(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	got := make(chan controller.Request, 4)
	p := pool.New(context.Background(), func(_ context.Context, req controller.Request) (controller.Outcome, error) {
		got <- req
		return controller.Outcome{}, nil
	}, 1, 4)
	cfg := &config.Config{
		DataDir: t.TempDir(),
		Limits:  config.Limits{MaxConcurrent: 1, TaskTimeout: "30m"},
		Repos:   []config.Repo{{Name: "app", Owner: "o", Repo: "app", DefaultBranch: "main", TokenRef: "gh"}},
		Agents:  map[string]config.AgentConfig{"mock": {Auth: "api_key", TokenRef: "model"}},
	}
	s := &submitter{cfg: cfg, store: st, pool: p}

	first, err := s.Submit(api.SubmitRequest{Repo: "app", Agent: "mock", Task: "split: Split the bill", Ref: "tab-split.p1.split.a1"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-got
	again, err := s.Submit(api.SubmitRequest{Repo: "app", Agent: "mock", Task: "split: Split the bill", Ref: "tab-split.p1.split.a1"})
	if err != nil {
		t.Fatalf("repeat submit: %v", err)
	}
	if again.TaskID != first.TaskID || !again.Existing || first.Existing {
		t.Errorf("repeat = %+v, first = %+v", again, first)
	}
	// Even after the task ended: the ref still names it.
	_ = st.UpdateTaskState(first.TaskID, store.StateFailed)
	done, err := s.Submit(api.SubmitRequest{Repo: "app", Agent: "mock", Task: "x", Ref: "tab-split.p1.split.a1"})
	if err != nil || done.TaskID != first.TaskID || !done.Existing {
		t.Errorf("after terminal: %+v, %v", done, err)
	}
	tasks, _ := st.ListTasks()
	if len(tasks) != 1 || tasks[0].Ref != "tab-split.p1.split.a1" {
		t.Fatalf("tasks = %+v, want the one task carrying the ref", tasks)
	}
	select {
	case req := <-got:
		t.Errorf("a repeat submit enqueued %s", req.TaskID)
	default:
	}
	// A different ref is a different task.
	second, err := s.Submit(api.SubmitRequest{Repo: "app", Agent: "mock", Task: "x", Ref: "tab-split.p1.split.a2"})
	if err != nil || second.Existing || second.TaskID == first.TaskID {
		t.Errorf("second ref: %+v, %v", second, err)
	}
	<-got
	for _, bad := range []string{"has space", "slash/inside", "colon:inside", strings.Repeat("x", 201)} {
		if _, err := s.Submit(api.SubmitRequest{Repo: "app", Agent: "mock", Task: "x", Ref: bad}); err == nil {
			t.Errorf("ref %q accepted", bad)
		}
	}
}

// Two submits with one ref at the same moment: the unique index lets one row
// in; the other resolves to it. Exactly one task exists afterwards.
func TestSubmitRefRace(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	p := pool.New(context.Background(), func(_ context.Context, req controller.Request) (controller.Outcome, error) {
		return controller.Outcome{}, nil
	}, 1, 64)
	cfg := &config.Config{
		DataDir: t.TempDir(),
		Limits:  config.Limits{MaxConcurrent: 1, TaskTimeout: "30m"},
		Repos:   []config.Repo{{Name: "app", Owner: "o", Repo: "app", DefaultBranch: "main", TokenRef: "gh"}},
		Agents:  map[string]config.AgentConfig{"mock": {Auth: "api_key", TokenRef: "model"}},
	}
	s := &submitter{cfg: cfg, store: st, pool: p}

	const n = 8
	results := make(chan api.SubmitResponse, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Submit(api.SubmitRequest{Repo: "app", Agent: "mock", Task: "x", Ref: "race.p1.item.a1"})
			if err != nil {
				t.Errorf("submit: %v", err)
				return
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	ids := map[string]bool{}
	fresh := 0
	for r := range results {
		ids[r.TaskID] = true
		if !r.Existing {
			fresh++
		}
	}
	if len(ids) != 1 || fresh != 1 {
		t.Errorf("ids = %v, fresh = %d; want one task filed once", ids, fresh)
	}
	tasks, _ := st.ListTasks()
	if len(tasks) != 1 {
		t.Errorf("tasks = %d, want 1", len(tasks))
	}
}
