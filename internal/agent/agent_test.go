package agent

import (
	"strings"
	"testing"
)

func TestLookup(t *testing.T) {
	for _, name := range []string{"claude", "mock"} {
		a, err := Lookup(name)
		if err != nil {
			t.Fatalf("lookup %q: %v", name, err)
		}
		if a.Name() != name {
			t.Errorf("Lookup(%q).Name() = %q", name, a.Name())
		}
	}
	if _, err := Lookup("nope"); err == nil {
		t.Error("expected error for unknown agent")
	}
}

func TestClaudeEnvVar(t *testing.T) {
	c := Claude()
	sub, _ := c.EnvVar(AuthSubscription)
	if sub != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Errorf("subscription env = %q", sub)
	}
	key, _ := c.EnvVar(AuthAPIKey)
	if key != "ANTHROPIC_API_KEY" {
		t.Errorf("api_key env = %q", key)
	}
	if _, err := c.EnvVar("bogus"); err == nil {
		t.Error("expected error for unknown auth method")
	}
}

func TestClaudeCommand(t *testing.T) {
	cmd, err := Claude().Command(AuthSubscription, "/task/prompt.md", "/task/out/transcript.json")
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	// The trailing pieces are the adapter-owned summary extraction (#36):
	// best-effort .result jq into summary.txt, then the agent status re-raised
	// so extraction never masks a claude failure.
	for _, want := range []string{
		"claude -p", "/task/prompt.md", "--output-format json", "--dangerously-skip-permissions", "/task/out/transcript.json",
		"jq -r '.result // empty'", "> /task/out/summary.txt", `(exit "$AGENT_STATUS")`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q:\n%s", want, cmd)
		}
	}
	// --bare would ignore the OAuth token subscription auth needs.
	if strings.Contains(cmd, "--bare") {
		t.Error("command uses --bare, incompatible with CLAUDE_CODE_OAUTH_TOKEN")
	}
}

func TestClaudeCommandRejectsBadAuth(t *testing.T) {
	if _, err := Claude().Command("bogus", "/p", "/t"); err == nil {
		t.Error("expected error for unsupported auth method")
	}
}

func TestMockCommandCommits(t *testing.T) {
	cmd, err := Mock().Command(AuthAPIKey, "/task/prompt.md", "/task/out/transcript.json")
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	for _, want := range []string{"/task/prompt.md", "mock_change.txt", "git commit", "> /task/out/summary.txt"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("mock command missing %q:\n%s", want, cmd)
		}
	}
}

func TestClaudeBudgetFlags(t *testing.T) {
	cmd, err := CommandFor(Claude(), AuthSubscription, "/task/prompt.md", "/task/out/transcript.json", Budget{MaxUSD: 2.5, MaxTurns: 40})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--max-budget-usd 2.5 ", "--max-turns 40 "} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command lacks %q:\n%s", want, cmd)
		}
	}
	plain, _ := Claude().Command(AuthSubscription, "/task/prompt.md", "/task/out/transcript.json")
	if strings.Contains(plain, "--max-") {
		t.Errorf("unbudgeted command carries a stop:\n%s", plain)
	}
}

func TestBudgetRefusedForAdapterWithoutStop(t *testing.T) {
	if _, err := CommandFor(Pi(), AuthAPIKey, "/p", "/t", Budget{MaxUSD: 1}); err == nil {
		t.Fatal("pi has no budget stop; CommandFor must refuse rather than run unbounded")
	}
	if _, err := CommandFor(Pi(), AuthAPIKey, "/p", "/t", Budget{}); err != nil {
		t.Fatalf("no budget must not need the stop: %v", err)
	}
}
