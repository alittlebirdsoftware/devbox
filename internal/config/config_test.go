package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The example config we ship must always be valid - this gaurds against
// comitting a broken config.example.yaml
func TestLoadExampleConfig(t *testing.T) {
	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("example config should load, got %v", err)
	}
	if len(cfg.Repos) == 0 {
		t.Fatalf("example config should have at least on repo")
	}
}

// Validation should reject broken configs. Table-driven: each entry is small
// YAML doc we expect Load to reject.
func TestLoadRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"no repos":               "repos: []\n",
		"duplicate name":         "repos:\n  - {name: a, owner: o, repo: r, token_ref: t}\n  - {name: a, owner: o, repo: r2, token_ref: t2}\n",
		"missing token_ref":      "repos:\n  - {name: a, owner: o, repo: r}\n",
		"bad timeout":            "limits: {task_timeout: banana}\nrepos:\n  - {name: a, owner: o, repo: r, token_ref: t}\n",
		"mcp not https":          "repos:\n  - {name: a, owner: o, repo: r, token_ref: t, mcp_servers: {artlist: http://mcp.artlist.io/mcp}}\n",
		"mcp creds path":         "repos:\n  - {name: a, owner: o, repo: r, token_ref: t, mcp_creds_ref: ../etc/passwd}\n",
		"check without contract": "repos:\n  - {name: a, owner: o, repo: r, token_ref: t, check: project-check}\n",
		"unknown contract":       "repos:\n  - {name: a, owner: o, repo: r, token_ref: t, result_contract: v2}\n",
		"check with quote":       "repos:\n  - {name: a, owner: o, repo: r, token_ref: t, result_contract: v1, check: \"echo 'x'\"}\n",
		"tiny memory":            "repos:\n  - {name: a, owner: o, repo: r, token_ref: t, limits: {memory_mb: 64}}\n",
		"bad cpus":               "repos:\n  - {name: a, owner: o, repo: r, token_ref: t, limits: {cpus: two}}\n",
		"bad repo timeout":       "repos:\n  - {name: a, owner: o, repo: r, token_ref: t, limits: {task_timeout: soon}}\n",
	}

	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTempConfig(t, doc)
			if _, err := Load(path); err == nil {
				t.Fatalf("expected an error for %q, got nil", name)
			}
		})
	}
}

// A minimal valid config should load and get defaults applied
func TestLoadAppliesDefaults(t *testing.T) {
	path := writeTempConfig(t, "repos:\n - {name: a, owner: o, repo: r, token_ref: t}\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("valid config failed to load: %v", err)
	}
	if cfg.Limits.MaxConcurrent != 2 {
		t.Errorf("max_concurrent default = %d, want 2", cfg.Limits.MaxConcurrent)
	}
	if cfg.Repos[0].DefaultBranch != "main" {
		t.Errorf("default_branch default = %q, want main", cfg.Repos[0].DefaultBranch)
	}
}

// writeTempConfig writes content to a temp file and returns its path
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadAgents(t *testing.T) {
	path := writeTempConfig(t, "repos:\n  - {name: a, owner: o, repo: r, token_ref: t}\n"+
		"agents:\n  claude: {auth: subscription, token_ref: claude-oauth-token}\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Agents["claude"].Auth != "subscription" || cfg.Agents["claude"].TokenRef != "claude-oauth-token" {
		t.Errorf("agent config not parsed: %+v", cfg.Agents["claude"])
	}
	if cfg.Image == "" || cfg.Podman == "" {
		t.Errorf("image/podman defaults not applied: image=%q podman=%q", cfg.Image, cfg.Podman)
	}
}

func TestLoadRejectsBadAgentAuth(t *testing.T) {
	path := writeTempConfig(t, "repos:\n  - {name: a, owner: o, repo: r, token_ref: t}\n"+
		"agents:\n  claude: {auth: bogus, token_ref: x}\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for bad agent auth")
	}
}

func TestLoadRepoMCP(t *testing.T) {
	doc := "repos:\n  - {name: a, owner: o, repo: r, token_ref: t, mcp_servers: {artlist: https://mcp.artlist.io/mcp}, mcp_creds_ref: claude-mcp-credentials}\n"
	cfg, err := Load(writeTempConfig(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Repos[0]
	if r.MCPServers["artlist"] != "https://mcp.artlist.io/mcp" || r.MCPCredsRef != "claude-mcp-credentials" {
		t.Fatalf("mcp fields not loaded: %+v", r)
	}
}

func TestRepoImageAndLimitsResolve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	doc := `image: localhost/devbox-agent-base:dev
limits: {task_timeout: 30m}
repos:
  - {name: hero, owner: o, repo: hero, token_ref: t}
  - name: app
    owner: o
    repo: app
    token_ref: t2
    image: localhost/devbox-flutter-amplify:dev
    limits: {cpus: "2", memory_mb: 4096, task_timeout: 45m}
    check: project-check
    result_contract: v1
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	hero, app := cfg.Repos[0], cfg.Repos[1]
	if got := cfg.TaskImage(hero); got != "localhost/devbox-agent-base:dev" {
		t.Errorf("hero image = %q, want the global image", got)
	}
	if cpus, mem, pids, to := cfg.TaskLimits(hero); cpus != "2" || mem != 2048 || pids != 256 || to.String() != "30m0s" {
		t.Errorf("hero limits = %s %d %d %s, want today's literals", cpus, mem, pids, to)
	}
	if got := cfg.TaskImage(app); got != "localhost/devbox-flutter-amplify:dev" {
		t.Errorf("app image = %q", got)
	}
	if cpus, mem, pids, to := cfg.TaskLimits(app); cpus != "2" || mem != 4096 || pids != 256 || to.String() != "45m0s" {
		t.Errorf("app limits = %s %d %d %s", cpus, mem, pids, to)
	}
}
