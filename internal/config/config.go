package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const DefaultSocketPath = "/run/agent-task/agent-task.sock"

// Defaults applied when a field is left unset in config.yaml
const (
	defaultDataDir       = "/var/lib/agent-task"
	defaultMaxConcurrent = 2
	defaultTaskTimeout   = "30m"
	defaultBranch        = "main"
	defaultImage         = "localhost/devbox-agent-base:dev"
	defaultPodman        = "podman"
)

// Config is the whole config.yaml, parsed into go
type Config struct {
	SocketPath string                 `yaml:"socket_path"`
	DataDir    string                 `yaml:"data_dir"`
	WorkDir    string                 `yaml:"work_dir"` // shared scratch root (agentbox-accessible); "" = data_dir/work
	Image      string                 `yaml:"image"`
	Podman     string                 `yaml:"podman"`
	Limits     Limits                 `yaml:"limits"`
	Repos      []Repo                 `yaml:"repos"`
	Agents     map[string]AgentConfig `yaml:"agents"`
	// CredentialsDir, when set, is read at use time instead of the LoadCredential
	// snapshot (see creds.UseDir): for credentials that rotate while the daemon runs.
	CredentialsDir string `yaml:"credentials_dir"`
}

// AgentConfig is the per-agent auth policy. token_ref names a LoadCredential
// secret (the model key); the secret itself never appears in config (D3).
type AgentConfig struct {
	Auth     string `yaml:"auth"`      // "subscription" | "api_key"
	TokenRef string `yaml:"token_ref"` // LoadCredential name for the model credential
	Model    string `yaml:"model"`     // default model for this agent; a repo's model overrides it
}

// Limits are the daemon's resource caps (D10)
// TaskTimeout is a string like "30m' here; we parse it in the validation step,
// because yaml.v3 can't turn "30m" into a time.Duration on it's own
type Limits struct {
	MaxConcurrent int    `yaml:"max_concurrent"`
	TaskTimeout   string `yaml:"task_timeout"`
}

var envNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// Repo is one entry in the static registry (D11): a short name mapped to a
// Github repo plus the LoadCredentials secret NAME (never the token itself).
type Repo struct {
	Name          string `yaml:"name"`
	Owner         string `yaml:"owner"`
	Repo          string `yaml:"repo"`
	DefaultBranch string `yaml:"default_branch"`
	TokenRef      string `yaml:"token_ref"`
	// MCPServers registers remote MCP servers (name -> https URL) for the agent
	// inside this repo's containers; written to $HOME/.claude.json at task start.
	MCPServers map[string]string `yaml:"mcp_servers"`
	// MCPCredsRef names a LoadCredential secret holding the agent's MCP OAuth
	// store (the mcpOAuth section of a claude .credentials.json), delivered by
	// env-file and written to $HOME/.claude/.credentials.json in the container.
	// The secret itself never appears in config (D3).
	MCPCredsRef string `yaml:"mcp_creds_ref"`
	// Model pins the agent's model for this repo (e.g. claude-sonnet-5 for image tasks,
	// claude-opus-5 for code). Empty = the agent's default (agents.<name>.model), else the CLI's default.
	Model string `yaml:"model"`
	// EnvSecrets maps an environment variable NAME to a LoadCredential secret NAME; the daemon resolves
	// the value host-side and delivers it by env-file (never argv, never config). Lets a task call a
	// keyed API (e.g. GEMINI_API_KEY for image generation) without an OAuth store.
	EnvSecrets map[string]string `yaml:"env_secrets"`
	// Image runs this repo's tasks in a project-type image instead of the global one. "" = the global image.
	Image string `yaml:"image"`
	// Limits overrides the per-task resource caps for this repo; unset fields keep the defaults
	// (2 CPUs, 2048 MB, 256 pids, the global task_timeout), so an unconfigured repo behaves as before.
	Limits RepoLimits `yaml:"limits"`
	// Check is a command the wrapper runs in the source tree after the agent exits, as
	// `<check> <scope> <active-set.json> <checks.json>`; it must write checks.json. Requires
	// result_contract v1. "" = no check (today's behaviour).
	Check string `yaml:"check"`
	// ResultContract "v1" derives the terminal state from the task verdict (controller.BuildVerdict):
	// checks, replan.json and blocked.json, and the agent's budget stop. "" = Completed iff exit 0 and ≥1 commit.
	ResultContract string `yaml:"result_contract"`
}

// RepoLimits are one repo's per-task resource caps. Zero values mean "the default".
type RepoLimits struct {
	CPUs        string `yaml:"cpus"`
	MemoryMB    int    `yaml:"memory_mb"`
	PidsLimit   int    `yaml:"pids_limit"`
	TaskTimeout string `yaml:"task_timeout"`
}

// Per-task defaults, used when neither the repo nor the global config sets a cap.
const (
	DefaultTaskCPUs      = "2"
	DefaultTaskMemoryMB  = 2048
	DefaultTaskPidsLimit = 256
)

// ResultContractV1 is the only result contract (see Repo.ResultContract).
const ResultContractV1 = "v1"

// TaskImage is the image this repo's tasks run in.
func (c *Config) TaskImage(r Repo) string {
	if r.Image != "" {
		return r.Image
	}
	return c.Image
}

// TaskLimits resolves this repo's caps over the defaults. The timeout was validated at load.
func (c *Config) TaskLimits(r Repo) (cpus string, memoryMB, pids int, timeout time.Duration) {
	cpus, memoryMB, pids = DefaultTaskCPUs, DefaultTaskMemoryMB, DefaultTaskPidsLimit
	if r.Limits.CPUs != "" {
		cpus = r.Limits.CPUs
	}
	if r.Limits.MemoryMB > 0 {
		memoryMB = r.Limits.MemoryMB
	}
	if r.Limits.PidsLimit > 0 {
		pids = r.Limits.PidsLimit
	}
	t := c.Limits.TaskTimeout
	if r.Limits.TaskTimeout != "" {
		t = r.Limits.TaskTimeout
	}
	timeout, _ = time.ParseDuration(t)
	return cpus, memoryMB, pids, timeout
}

var cpusRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

func (r Repo) validateBuildFields() error {
	switch {
	case r.Limits.CPUs != "" && !cpusRE.MatchString(r.Limits.CPUs):
		return fmt.Errorf("repo %q: limits.cpus must be a number, got %q", r.Name, r.Limits.CPUs)
	case r.Limits.MemoryMB < 0 || (r.Limits.MemoryMB > 0 && r.Limits.MemoryMB < 256):
		return fmt.Errorf("repo %q: limits.memory_mb must be at least 256, got %d", r.Name, r.Limits.MemoryMB)
	case r.Limits.PidsLimit < 0 || (r.Limits.PidsLimit > 0 && r.Limits.PidsLimit < 64):
		return fmt.Errorf("repo %q: limits.pids_limit must be at least 64, got %d", r.Name, r.Limits.PidsLimit)
	case r.ResultContract != "" && r.ResultContract != ResultContractV1:
		return fmt.Errorf("repo %q: result_contract must be v1 or empty, got %q", r.Name, r.ResultContract)
	case r.Check != "" && r.ResultContract != ResultContractV1:
		return fmt.Errorf("repo %q: check requires result_contract v1", r.Name)
	case strings.ContainsAny(r.Check, "\n'"):
		return fmt.Errorf("repo %q: check must be one line without single quotes", r.Name)
	}
	if r.Limits.TaskTimeout != "" {
		if _, err := time.ParseDuration(r.Limits.TaskTimeout); err != nil {
			return fmt.Errorf("repo %q: limits.task_timeout %q is not a valid duration: %w", r.Name, r.Limits.TaskTimeout, err)
		}
	}
	return nil
}

// Load reads and parses the YAML config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}

	return &cfg, nil
}

// applyDefaults fills unset fields with sensible defaults
func (c *Config) applyDefaults() {
	if c.SocketPath == "" {
		c.SocketPath = DefaultSocketPath
	}

	if c.DataDir == "" {
		c.DataDir = defaultDataDir
	}

	if c.Limits.MaxConcurrent == 0 {
		c.Limits.MaxConcurrent = defaultMaxConcurrent
	}

	if c.Limits.TaskTimeout == "" {
		c.Limits.TaskTimeout = defaultTaskTimeout
	}

	if c.Image == "" {
		c.Image = defaultImage
	}

	if c.Podman == "" {
		c.Podman = defaultPodman
	}

	// range give a COPY of each element, so index by i to mutate the slice
	for i := range c.Repos {
		if c.Repos[i].DefaultBranch == "" {
			c.Repos[i].DefaultBranch = defaultBranch
		}
	}
}

// Validate checks the config is complete and internally consistent
func (c *Config) Validate() error {
	if len(c.Repos) == 0 {
		return fmt.Errorf("no repos configured: the registry needs at least one entry")
	}

	if _, err := time.ParseDuration(c.Limits.TaskTimeout); err != nil {
		return fmt.Errorf("limits.task_timeout %q is not a valid duration: %w", c.Limits.TaskTimeout, err)
	}

	if c.CredentialsDir != "" && !strings.HasPrefix(c.CredentialsDir, "/") {
		return fmt.Errorf("credentials_dir must be an absolute path, got %q", c.CredentialsDir)
	}

	if c.Limits.MaxConcurrent < 1 {
		return fmt.Errorf("limits.max_concurrent must be >= 1, got %d", c.Limits.MaxConcurrent)
	}

	seen := make(map[string]bool)
	for i, r := range c.Repos {
		switch {
		case r.Name == "":
			return fmt.Errorf("repos[%d]: name is required", i)
		case seen[r.Name]:
			return fmt.Errorf("repos[%d]: duplicate repo name %q", i, r.Name)
		case r.Owner == "" || r.Repo == "":
			return fmt.Errorf("repo %q: owner and repo are required", r.Name)
		case r.TokenRef == "":
			return fmt.Errorf("repo %q: token_ref is required (the LoadCredential secret name)", r.Name)
		}
		for name, url := range r.MCPServers {
			if name == "" || !strings.HasPrefix(url, "https://") {
				return fmt.Errorf("repo %q: mcp_servers[%q] must be an https:// URL, got %q", r.Name, name, url)
			}
		}
		if strings.ContainsAny(r.MCPCredsRef, `/\`) {
			return fmt.Errorf("repo %q: mcp_creds_ref must be a bare LoadCredential name", r.Name)
		}
		for env, ref := range r.EnvSecrets {
			if !envNameRE.MatchString(env) {
				return fmt.Errorf("repo %q: env_secrets key %q must be an environment variable name", r.Name, env)
			}
			if ref == "" || strings.ContainsAny(ref, `/\`) {
				return fmt.Errorf("repo %q: env_secrets[%q] must be a bare LoadCredential name", r.Name, env)
			}
		}
		if err := r.validateBuildFields(); err != nil {
			return err
		}
		seen[r.Name] = true
	}

	for name, a := range c.Agents {
		switch a.Auth {
		case "subscription", "api_key":
		default:
			return fmt.Errorf("agent %q: auth must be subscription or api_key, got %q", name, a.Auth)
		}
		if a.TokenRef == "" {
			return fmt.Errorf("agent %q: token_ref is required", name)
		}
	}
	return nil
}
