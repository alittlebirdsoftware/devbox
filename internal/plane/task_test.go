package plane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "agent", "testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

func TestFromRunClaude(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "transcript.json"), fixture(t, "claude/ok.json"), 0o644)
	os.WriteFile(filepath.Join(dir, "agent_version.txt"), []byte("2.0.1 (Claude Code)\n"), 0o644)
	start := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	got := FromRun(dir, Facts{TaskID: "t1", IssueNumber: 7, AgentName: "claude", ContainerID: "agent-task-t1",
		BundleSha: "abc1234", StartedAt: start, FinishedAt: start.Add(42 * time.Second), Commits: 2, ExitCode: 0, PROpened: true})
	if got.Iterations != 6 || got.CostUSD != 0.0512 {
		t.Errorf("transcript not applied: %+v", got)
	}
	if got.Agent.Version != "2.0.1 (Claude Code)" || got.Agent.Name != "claude" {
		t.Errorf("agent: %+v", got.Agent)
	}
	if got.Outcome != "pr_opened" || got.StartedAt != "2026-09-05T10:00:00Z" {
		t.Errorf("facts: %+v", got)
	}
}

func TestFromRunPiTranscriptYieldsZeros(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "transcript.json"), fixture(t, "pi/ok.jsonl"), 0o644)
	got := FromRun(dir, Facts{TaskID: "t2", AgentName: "pi", ExitCode: 0, Commits: 0})
	if got.Iterations != 0 || got.Usage.InputTokens != 0 || got.Agent.Version != "unknown" {
		t.Errorf("expected zeros for a JSON-lines transcript: %+v", got)
	}
	if got.Outcome != "no_change" {
		t.Errorf("outcome: %s", got.Outcome)
	}
}

func TestOutcomeFailed(t *testing.T) {
	got := FromRun(t.TempDir(), Facts{TaskID: "t3", AgentName: "claude", ExitCode: 1, Commits: 0})
	if got.Outcome != "failed" {
		t.Errorf("outcome: %s", got.Outcome)
	}
}

func TestCommentCannotCloseItself(t *testing.T) {
	tk := FromRun(t.TempDir(), Facts{TaskID: "t--> <script>", AgentName: "claude"})
	c := Comment(tk)
	if !strings.HasPrefix(c, "<!-- "+Marker+" ") || !strings.HasSuffix(c, " -->") {
		t.Errorf("shape: %s", c)
	}
	if strings.Count(c, "-->") != 1 {
		t.Errorf("JSON leaked a comment terminator: %s", c)
	}
}

func TestTranscriptModelIsTheOneThatDidTheWork(t *testing.T) {
	var task Task
	applyClaudeTranscript(&task, []byte(`{"num_turns":46,"total_cost_usd":1.07,"usage":{"input_tokens":1,"output_tokens":2},
	  "modelUsage":{"claude-haiku-4-5-20251001":{"outputTokens":18},"claude-sonnet-5":{"outputTokens":21939}}}`))
	if task.Agent.Model != "claude-sonnet-5" {
		t.Fatalf("expected the dominant model, got %q", task.Agent.Model)
	}
}
