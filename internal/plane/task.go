// Package plane emits the control plane's agent-side task facts. They ride in
// the PR body as an HTML comment so the gate workflow — which holds the ledger
// credentials this host deliberately does not (0003) — can write the Task row.
// Every field is something the host observed; nothing is the agent's opinion
// of itself.
package plane

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Marker is the HTML-comment tag the gate greps for in the PR body.
const Marker = "plane-task"

// Agent identifies the coding agent that ran.
type Agent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Model   string `json:"model"`
}

// Usage is model token usage as reported by the agent's transcript.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

// Task is the agent-side half of a control-plane Task row.
type Task struct {
	TaskID      string  `json:"taskId"`
	IssueNumber int     `json:"issueNumber,omitempty"`
	Agent       Agent   `json:"agent"`
	Runner      string  `json:"runner"`
	ContainerID string  `json:"containerId"`
	BundleSha   string  `json:"bundleSha,omitempty"`
	StartedAt   string  `json:"startedAt"`
	FinishedAt  string  `json:"finishedAt"`
	Usage       Usage   `json:"usage"`
	Iterations  int     `json:"iterations"`
	Outcome     string  `json:"outcome"` // pr_opened | no_change | failed
	Commits     int     `json:"commits"`
	ExitCode    int     `json:"exitCode"`
	CostUSD     float64 `json:"costUsd,omitempty"`
}

// Facts are the host-observed inputs to FromRun.
type Facts struct {
	TaskID      string
	IssueNumber int
	AgentName   string
	ContainerID string
	BundleSha   string
	StartedAt   time.Time
	FinishedAt  time.Time
	Commits     int
	ExitCode    int
	PROpened    bool
}

// FromRun assembles the task facts from the run's out dir. Transcript parsing
// is best-effort and agent-shaped: Claude's --output-format json is one object
// carrying num_turns, total_cost_usd, usage and (newer builds) modelUsage keyed
// by model id; Pi's JSON-lines transcript is not one object and yields zeros.
// agent_version.txt is written by the adapter command when the CLI supports
// --version.
func FromRun(outDir string, f Facts) Task {
	t := Task{
		TaskID: f.TaskID, IssueNumber: f.IssueNumber,
		Agent:  Agent{Name: f.AgentName, Version: "unknown", Model: "unknown"},
		Runner: "podman", ContainerID: f.ContainerID, BundleSha: f.BundleSha,
		StartedAt:  f.StartedAt.UTC().Format(time.RFC3339),
		FinishedAt: f.FinishedAt.UTC().Format(time.RFC3339),
		Commits:    f.Commits, ExitCode: f.ExitCode,
	}
	switch {
	case f.PROpened:
		t.Outcome = "pr_opened"
	case f.ExitCode == 0 && f.Commits == 0:
		t.Outcome = "no_change"
	default:
		t.Outcome = "failed"
	}
	if v := readTrimmed(filepath.Join(outDir, "agent_version.txt")); v != "" {
		t.Agent.Version = firstLine(v)
	}
	if b, err := os.ReadFile(filepath.Join(outDir, "transcript.json")); err == nil {
		applyClaudeTranscript(&t, b)
	}
	return t
}

func applyClaudeTranscript(t *Task, b []byte) {
	var tr struct {
		NumTurns int     `json:"num_turns"`
		CostUSD  float64 `json:"total_cost_usd"`
		Usage    struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
		ModelUsage map[string]json.RawMessage `json:"modelUsage"`
	}
	if err := json.Unmarshal(b, &tr); err != nil {
		return // not a single JSON object (e.g. Pi's JSON lines): leave zeros
	}
	t.Iterations = tr.NumTurns
	t.CostUSD = tr.CostUSD
	t.Usage = Usage{InputTokens: tr.Usage.Input, OutputTokens: tr.Usage.Output}
	if len(tr.ModelUsage) > 0 {
		// The model that did the work: most output tokens. Claude Code also makes tiny helper
		// calls on a small model (a title, 18 tokens), which must not be reported as the agent.
		keys := make([]string, 0, len(tr.ModelUsage))
		for k := range tr.ModelUsage {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		best, bestOut := keys[0], -1
		for _, k := range keys {
			var u struct {
				Output int `json:"outputTokens"`
			}
			_ = json.Unmarshal(tr.ModelUsage[k], &u)
			if u.Output > bestOut {
				best, bestOut = k, u.Output
			}
		}
		t.Agent.Model = best
	}
}

// JSON renders the facts on one line. encoding/json escapes <, > and & as
// <, >, &, so the result can never close an HTML comment.
func (t Task) JSON() string {
	b, _ := json.Marshal(t)
	return string(b)
}

// Comment renders the hidden PR-body block the gate extracts.
func Comment(t Task) string { return "<!-- " + Marker + " " + t.JSON() + " -->" }

func readTrimmed(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
