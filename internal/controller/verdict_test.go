package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iQonAi/devbox/internal/agent"
	"github.com/iQonAi/devbox/internal/prompt"
	"github.com/iQonAi/devbox/internal/repo"
	"github.com/iQonAi/devbox/internal/runner"
)

func TestBuildVerdictPrecedence(t *testing.T) {
	valid := resultFile{Present: true, Valid: true}
	passing := checkResult{Ran: true, Exit: 0, OK: true, Scope: "ticket:w1"}
	base := VerdictInput{ExitCode: 0, Commits: 1, Scope: "ticket:w1", HasCheck: true, Check: passing}
	with := func(f func(*VerdictInput)) VerdictInput { in := base; f(&in); return in }

	cases := []struct {
		name string
		in   VerdictInput
		want Verdict
	}{
		{"passing change", base, VerdictChange},
		{"no check configured", with(func(in *VerdictInput) { in.HasCheck, in.Check = false, checkResult{} }), VerdictChange},
		{"cancel beats everything", with(func(in *VerdictInput) { in.Ended, in.BudgetStop = "cancelled", true }), VerdictCancelled},
		{"timeout beats budget", with(func(in *VerdictInput) { in.Ended, in.BudgetStop = "timeout", true }), VerdictTimedOut},
		{"shutdown is failed", with(func(in *VerdictInput) { in.Ended = "shutdown" }), VerdictFailed},
		{"budget beats a passing commit", with(func(in *VerdictInput) { in.BudgetStop = true }), VerdictBudgetExceeded},
		{"budget beats a non-zero exit", with(func(in *VerdictInput) { in.BudgetStop, in.ExitCode = true, 1 }), VerdictBudgetExceeded},
		{"non-zero exit", with(func(in *VerdictInput) { in.ExitCode = 2 }), VerdictFailed},
		{"blocked", with(func(in *VerdictInput) { in.Commits, in.Blocked = 0, valid }), VerdictBlocked},
		{"replan", with(func(in *VerdictInput) { in.Commits, in.Replan = 0, valid }), VerdictReplan},
		{"blocked with commits", with(func(in *VerdictInput) { in.Blocked = valid }), VerdictFailed},
		{"two result artifacts", with(func(in *VerdictInput) { in.Commits, in.Blocked, in.Replan = 0, valid, valid }), VerdictFailed},
		{"invalid result artifact", with(func(in *VerdictInput) { in.Commits, in.Replan = 0, resultFile{Present: true} }), VerdictFailed},
		{"no commits", with(func(in *VerdictInput) { in.Commits = 0 }), VerdictFailed},
		{"check did not run", with(func(in *VerdictInput) { in.Check = checkResult{} }), VerdictFailed},
		{"check exit non-zero", with(func(in *VerdictInput) { in.Check.Exit = 1 }), VerdictFailed},
		{"checks.json not ok", with(func(in *VerdictInput) { in.Check.OK = false }), VerdictFailed},
		{"check ran another scope", with(func(in *VerdictInput) { in.Check.Scope = "scaffold" }), VerdictFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, reason := BuildVerdict(tc.in); got != tc.want {
				t.Errorf("verdict = %s (%s), want %s", got, reason, tc.want)
			}
		})
	}
}

func TestReadVerdictInputs(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("check.exit", "0\n")
	write("checks.json", `{"ok":true,"scope":"regression"}`)
	write("replan.json", `{"reason":"the model needs a Course entity"}`)
	write("blocked.json", `{"note":"no reason field"}`)
	write("transcript.json", `{"type":"result","subtype":"error_max_budget_usd"}`)
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "linked.json")); err != nil {
		t.Fatal(err)
	}

	var in VerdictInput
	readVerdictInputs(dir, &in)
	if !in.Check.Ran || in.Check.Exit != 0 || !in.Check.OK || in.Check.Scope != "regression" {
		t.Errorf("check = %+v", in.Check)
	}
	if !in.Replan.Present || !in.Replan.Valid {
		t.Errorf("replan = %+v, want present and valid", in.Replan)
	}
	if !in.Blocked.Present || in.Blocked.Valid {
		t.Errorf("blocked = %+v, want present and invalid (no reason)", in.Blocked)
	}
	if !in.BudgetStop {
		t.Error("budget stop not read from the transcript subtype")
	}
	if got := readResultFile(filepath.Join(dir, "linked.json")); got.Valid {
		t.Error("a symlinked result artifact must never be valid")
	}
}

// checkingRunner is localRunner plus what the v1 wrapper leaves behind: a
// commit, and the check's check.exit and checks.json.
type checkingRunner struct {
	localRunner
	checkExit, checksJSON string
	checkEnv              *map[string]string
}

func (c checkingRunner) Run(ctx context.Context, spec runner.Spec) (runner.Result, error) {
	res, err := c.localRunner.Run(ctx, spec)
	if err != nil {
		return res, err
	}
	*c.checkEnv = spec.SecretEnv
	if c.checkExit != "" {
		_ = os.WriteFile(filepath.Join(spec.OutDir, "check.exit"), []byte(c.checkExit), 0o644)
		_ = os.WriteFile(filepath.Join(spec.OutDir, "checks.json"), []byte(c.checksJSON), 0o644)
	}
	return res, nil
}

func runV1(t *testing.T, r runner.Runner) Outcome {
	t.Helper()
	origin := initOrigin(t)
	out, err := Run(context.Background(),
		Deps{Repo: repo.NewManager(t.TempDir()), Runner: r, Image: "global"},
		Request{
			TaskID: "task-v1", Title: "w1", RepoName: "devbox",
			RepoURL: "file://" + origin, DefaultBranch: "main",
			Prompt: prompt.Input{Task: "implement w1"}, Agent: agent.Mock(), AuthMethod: agent.AuthAPIKey,
			ResultContract: "v1", Check: "project-check", CheckScope: "ticket:w1",
			ActiveSet: `{"active": ["f1.1.v1"]}`,
			WorkDir:   t.TempDir(),
		})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return out
}

func TestRunV1PassingCheckCompletes(t *testing.T) {
	env := map[string]string{}
	out := runV1(t, checkingRunner{checkExit: "0\n", checksJSON: `{"ok":true,"scope":"ticket:w1"}`, checkEnv: &env})
	if out.State != StateCompleted || out.Verdict != VerdictChange {
		t.Fatalf("state/verdict = %s/%s (%s), want Completed/change", out.State, out.Verdict, out.Error)
	}
	if env[EnvCheckScope] != "ticket:w1" || env[EnvCheckActive] != `{"active":["f1.1.v1"]}` {
		t.Errorf("check env = %q / %q", env[EnvCheckScope], env[EnvCheckActive])
	}
	pt, err := os.ReadFile(filepath.Join(out.OutDir, "plane-task.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pt), `"buildResult":"change"`) || !strings.Contains(string(pt), `"scope":"ticket:w1"`) {
		t.Errorf("plane-task.json lacks the verdict: %s", pt)
	}
}

func TestRunV1FailingCheckIsFailedWithoutPR(t *testing.T) {
	env := map[string]string{}
	out := runV1(t, checkingRunner{checkExit: "1\n", checksJSON: `{"ok":false,"scope":"ticket:w1","failed":["f1.1.v1"]}`, checkEnv: &env})
	if out.State != StateFailed || out.Verdict != VerdictFailed || out.PRURL != "" {
		t.Fatalf("state/verdict/pr = %s/%s/%q", out.State, out.Verdict, out.PRURL)
	}
	if out.Error != "failed: checks failed" {
		t.Errorf("error = %q", out.Error)
	}
	var kinds []string
	for _, a := range out.Artifacts {
		kinds = append(kinds, a.Kind)
	}
	if !strings.Contains(strings.Join(kinds, ","), "checks") {
		t.Errorf("checks.json not collected as an artifact: %v", kinds)
	}
}

func TestRunV1RequestImageOverridesDeps(t *testing.T) {
	var got string
	r := specSpy{fn: func(s runner.Spec) { got = s.Image }}
	origin := initOrigin(t)
	_, _ = Run(context.Background(),
		Deps{Repo: repo.NewManager(t.TempDir()), Runner: r, Image: "global"},
		Request{
			TaskID: "task-img", Title: "x", RepoName: "devbox", RepoURL: "file://" + origin, DefaultBranch: "main",
			Prompt: prompt.Input{Task: "t"}, Agent: agent.Mock(), AuthMethod: agent.AuthAPIKey,
			Image: "localhost/devbox-flutter-amplify:dev", WorkDir: t.TempDir(),
		})
	if got != "localhost/devbox-flutter-amplify:dev" {
		t.Errorf("image = %q", got)
	}
}

type specSpy struct{ fn func(runner.Spec) }

func (s specSpy) Run(_ context.Context, spec runner.Spec) (runner.Result, error) {
	s.fn(spec)
	return runner.Result{ExitCode: 1}, nil
}
