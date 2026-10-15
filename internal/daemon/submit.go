package daemon

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/iQonAi/devbox/internal/agent"
	"github.com/iQonAi/devbox/internal/api"
	"github.com/iQonAi/devbox/internal/config"
	"github.com/iQonAi/devbox/internal/controller"
	"github.com/iQonAi/devbox/internal/creds"
	"github.com/iQonAi/devbox/internal/pool"
	"github.com/iQonAi/devbox/internal/prompt"
	"github.com/iQonAi/devbox/internal/store"
)

// submitter builds a controller.Request from an API submission — resolving
// secrets host-side via LoadCredential (D3) — persists the task, and enqueues
// it on the pool. It implements api.Submitter.
type submitter struct {
	cfg   *config.Config
	store *store.Store
	pool  *pool.Pool
}

func (s *submitter) Submit(sr api.SubmitRequest) (api.SubmitResponse, error) {
	if sr.Task == "" && sr.Issue == 0 {
		return api.SubmitResponse{}, fmt.Errorf("one of task or issue is required")
	}
	if sr.Task != "" && sr.Issue != 0 {
		return api.SubmitResponse{}, fmt.Errorf("task and issue are mutually exclusive")
	}
	if sr.Ref != "" && !refRE.MatchString(sr.Ref) {
		return api.SubmitResponse{}, fmt.Errorf("ref %q: want 1-200 characters of [A-Za-z0-9._-]", sr.Ref)
	}
	// Idempotent by ref: the task this host already has for it is the answer,
	// whatever state it is in. Checked before any validation that depends on
	// today's config, so a repeat resolves even if the registry changed since.
	if existing, err := s.store.TaskByRef(sr.Ref); err != nil {
		return api.SubmitResponse{}, err
	} else if existing != nil {
		return api.SubmitResponse{TaskID: existing.ID, Existing: true}, nil
	}
	id, err := s.submitNew(sr)
	if err != nil {
		if errors.Is(err, store.ErrDuplicateRef) {
			// Two submits with one ref raced to the insert; the index let one through. Answer with that one.
			if existing, lerr := s.store.TaskByRef(sr.Ref); lerr == nil && existing != nil {
				return api.SubmitResponse{TaskID: existing.ID, Existing: true}, nil
			}
		}
		return api.SubmitResponse{}, err
	}
	return api.SubmitResponse{TaskID: id}, nil
}

// refRE bounds a client reference to the characters a container name, a path
// and a log line all tolerate.
var refRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)

// submitNew files a task: resolves the repo and agent, the secrets host-side
// via LoadCredential (D3), persists the row (with the ref), and enqueues it.
func (s *submitter) submitNew(sr api.SubmitRequest) (string, error) {

	var rc *config.Repo
	for i := range s.cfg.Repos {
		if s.cfg.Repos[i].Name == sr.Repo {
			rc = &s.cfg.Repos[i]
			break
		}
	}
	if rc == nil {
		return "", fmt.Errorf("unknown repo %q", sr.Repo)
	}

	ag, err := agent.Lookup(sr.Agent)
	if err != nil {
		return "", err
	}
	if err := validateBuildSubmit(sr, *rc, ag); err != nil {
		return "", err
	}
	ac, ok := s.cfg.Agents[sr.Agent]
	if !ok {
		return "", fmt.Errorf("agent %q not configured", sr.Agent)
	}

	// Resolve secrets from LoadCredential (host-only). Absent = empty (anon clone
	// / no model auth), which the downstream steps surface as a clear failure.
	repoToken, _, err := creds.Get(rc.TokenRef)
	if err != nil {
		return "", err
	}
	modelToken, _, err := creds.Get(ac.TokenRef)
	if err != nil {
		return "", err
	}
	mcpCreds := ""
	if rc.MCPCredsRef != "" {
		mcpCreds, _, err = creds.Get(rc.MCPCredsRef)
		if err != nil {
			return "", err
		}
	}
	envSecrets := map[string]string{}
	for name, ref := range rc.EnvSecrets {
		v, _, err := creds.Get(ref)
		if err != nil {
			return "", err
		}
		envSecrets[name] = v
	}

	if err := s.store.UpsertRepo(store.Repo{
		Name: rc.Name, Owner: rc.Owner, Repo: rc.Repo,
		DefaultBranch: rc.DefaultBranch, TokenRef: rc.TokenRef,
	}); err != nil {
		return "", err
	}
	repoID, err := s.repoID(rc.Name)
	if err != nil {
		return "", err
	}

	// UnixNano alone can collide (clock steps, parallel submits); a random
	// suffix makes the id unique. Hex keeps it inside the container-name
	// charset ([a-z0-9-], see runner's name validation).
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("task id suffix: %w", err)
	}
	taskID := fmt.Sprintf("t%d-%x", time.Now().UnixNano(), suffix)
	source := "task"
	if sr.Issue > 0 {
		source = "issue"
	}
	if err := s.store.CreateTask(store.NewTask{
		ID: taskID, RepoID: repoID, Source: source, Agent: sr.Agent, Ref: sr.Ref,
	}); err != nil {
		return "", err
	}
	_ = s.store.InsertEvent(taskID, store.EventState, "Created")

	// Per-task scratch under the shared work root (agentbox-accessible; the root
	// is setgid to a shared group so both the daemon uid and the container uid
	// reach it). 2770 + setgid propagates the group to podman-written files, so
	// the daemon can read back what agentbox wrote. Falls back to data_dir/work
	// for local/test runs where the two uids are the same.
	workRoot := s.cfg.WorkDir
	if workRoot == "" {
		workRoot = filepath.Join(s.cfg.DataDir, "work")
	}
	workDir := filepath.Join(workRoot, taskID)
	for _, d := range []string{workDir, filepath.Join(workDir, "out")} {
		if err := os.MkdirAll(d, 0o770); err != nil {
			return "", err
		}
		if err := os.Chmod(d, os.ModeSetgid|0o770); err != nil {
			return "", err
		}
	}

	cpus, memoryMB, pids, timeout := s.cfg.TaskLimits(*rc)
	req := controller.Request{
		TaskID:   taskID,
		Title:    sr.Task,
		RepoName: rc.Name,
		RepoURL:  rc.CloneURL(),
		Owner:    rc.Owner, Repo: rc.Repo, DefaultBranch: rc.DefaultBranch,
		RepoHost: rc.Host, RepoAPIURL: rc.APIURL,
		IssueNumber: sr.Issue, RepoToken: repoToken,
		Prompt: prompt.Input{Task: sr.Task},
		Agent:  ag, AuthMethod: agent.AuthMethod(ac.Auth), AuthValue: modelToken,
		MCPServers: rc.MCPServers, MCPCreds: mcpCreds, EnvSecrets: envSecrets,
		Model:          firstNonEmpty(sr.Model, rc.Model, ac.Model),
		Image:          s.cfg.TaskImage(*rc),
		Budget:         agent.Budget{MaxUSD: sr.BudgetUSD, MaxTurns: sr.MaxTurns},
		ResultContract: rc.ResultContract, Check: rc.Check,
		CheckScope: sr.Scope, ActiveSet: string(sr.ActiveSet),
		WorkDir: workDir,
		Limits:  controller.Limits{CPUs: cpus, MemoryMB: memoryMB, PidsLimit: pids, Timeout: timeout},
	}
	if err := s.pool.Submit(req); err != nil {
		_ = s.store.UpdateTaskState(taskID, store.StateFailed)
		_ = s.store.InsertEvent(taskID, store.EventState, "->Failed: "+err.Error())
		return "", err
	}
	return taskID, nil
}

func (s *submitter) Cancel(taskID string) error {
	found, queued := s.pool.Cancel(taskID)
	if !found {
		return fmt.Errorf("task %q is not running", taskID)
	}
	_ = s.store.InsertEvent(taskID, store.EventSecurity, "cancel requested")
	if queued {
		// The task never started (the pool skips it), so nothing else will
		// record its terminal state — do it here (§7.1: cancel before start).
		_ = s.store.UpdateTaskState(taskID, store.StateCancelled)
		_ = s.store.InsertEvent(taskID, store.EventState, "Created->Cancelled: cancelled before start")
	}
	return nil
}

func (s *submitter) repoID(name string) (int64, error) {
	repos, err := s.store.ListRepos()
	if err != nil {
		return 0, err
	}
	for _, r := range repos {
		if r.Name == name {
			return r.ID, nil
		}
	}
	return 0, fmt.Errorf("repo %q not found after upsert", name)
}

var scopeRE = regexp.MustCompile(`^(scaffold|regression|(ticket|milestone):[A-Za-z0-9._-]+)$`)

// validateBuildSubmit refuses a submission whose v1 fields do not fit the repo:
// a scope exactly when the repo has a check, a JSON-object active set only with
// a scope, and a budget only for an adapter that enforces one.
func validateBuildSubmit(sr api.SubmitRequest, rc config.Repo, ag agent.Agent) error {
	switch {
	case rc.Check != "" && sr.Scope == "":
		return fmt.Errorf("repo %q has a check: scope is required", rc.Name)
	case rc.Check == "" && (sr.Scope != "" || len(sr.ActiveSet) > 0):
		return fmt.Errorf("repo %q has no check: scope and activeSet are not accepted", rc.Name)
	case sr.Scope != "" && !scopeRE.MatchString(sr.Scope):
		return fmt.Errorf("scope %q: want scaffold, regression, ticket:<id> or milestone:<id>", sr.Scope)
	case sr.BudgetUSD < 0 || sr.MaxTurns < 0:
		return fmt.Errorf("budgetUsd and maxTurns must not be negative")
	case (sr.BudgetUSD > 0 || sr.MaxTurns > 0) && !agent.SupportsBudget(ag):
		return fmt.Errorf("agent %q does not support a budget stop", ag.Name())
	}
	if len(sr.ActiveSet) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(sr.ActiveSet, &obj); err != nil {
			return fmt.Errorf("activeSet must be a JSON object: %w", err)
		}
		if sr.Scope == "scaffold" && len(obj["active"]) > 0 && string(obj["active"]) != "[]" {
			return fmt.Errorf("scope scaffold takes an empty active set")
		}
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
