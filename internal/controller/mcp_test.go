package controller

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPRegistrationJSON(t *testing.T) {
	got, err := mcpRegistrationJSON(map[string]string{"artlist": "https://mcp.artlist.io/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	var reg map[string]map[string]map[string]string
	if err := json.Unmarshal([]byte(got), &reg); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if reg["mcpServers"]["artlist"]["type"] != "http" || reg["mcpServers"]["artlist"]["url"] != "https://mcp.artlist.io/mcp" {
		t.Fatalf("unexpected registration %s", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("registration must be one line for the env-file: %q", got)
	}
}

func TestCompactJSONIsOneLine(t *testing.T) {
	in := "{\n  \"mcpOAuth\": {\n    \"artlist|x\": {\"accessToken\": \"a\"}\n  }\n}\n"
	out := compactJSON(in)
	if strings.ContainsAny(out, "\n ") {
		t.Fatalf("not compact: %q", out)
	}
}

func TestWrapperSeedsHomeFromEnv(t *testing.T) {
	cmd := wrapperCmd("deadbeef", "true")
	script := cmd[len(cmd)-1]
	for _, want := range []string{
		"$" + EnvMCPServers, "\"$HOME/.claude.json\"",
		"$" + EnvMCPCreds, "\"$HOME/.claude/.credentials.json\"",
		"unset " + EnvMCPServers, "unset " + EnvMCPCreds, "umask 077",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("wrapper script lacks %q:\n%s", want, script)
		}
	}
	if !strings.Contains(script, "( umask 077; cp") {
		t.Fatalf("credential hand-back must set umask in a subshell so the bundle stays readable:\n%s", script)
	}
	if !strings.Contains(script, "/claude-credentials.json") {
		t.Fatalf("wrapper must hand the refreshed credential store back as an artifact:\n%s", script)
	}
	// the seed block must run before the agent command
	if strings.Index(script, EnvMCPCreds) > strings.Index(script, "\ntrue\n") {
		t.Fatalf("home seeding must precede the agent command:\n%s", script)
	}
}
