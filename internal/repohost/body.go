package repohost

import (
	"fmt"
	"strings"
)

// ChangeInfo is the data rendered into the change body: a pull request body on
// GitHub, a merge request description on GitLab. The same text on both.
type ChangeInfo struct {
	TaskID     string
	Agent      string
	IssueURL   string // "" for a --task run
	Summary    string // the agent's summary (from the run's summary artifact)
	TestOutput string // optional captured test output; "" to omit
	PlaneTask  string // rendered plane.Comment(...); "" to omit. Hidden in the rendered body.
}

// BuildChangeBody renders the deterministic change body. It ends with a plain,
// no-emoji safety marker: the change is the only human approval gate (0001), so a
// reviewer must know the change is agent-authored and unmerged.
func BuildChangeBody(info ChangeInfo) string {
	var b strings.Builder
	b.WriteString("## Agent task\n\n")
	fmt.Fprintf(&b, "- Task: `%s`\n", info.TaskID)
	fmt.Fprintf(&b, "- Agent: `%s`\n", info.Agent)
	if info.IssueURL != "" {
		fmt.Fprintf(&b, "- Issue: %s\n", info.IssueURL)
	}

	b.WriteString("\n## Summary\n\n")
	if s := strings.TrimSpace(info.Summary); s != "" {
		b.WriteString(s)
	} else {
		b.WriteString("_(no summary produced)_")
	}
	b.WriteString("\n")

	if t := strings.TrimSpace(info.TestOutput); t != "" {
		b.WriteString("\n## Test results\n\n```\n")
		b.WriteString(t)
		b.WriteString("\n```\n")
	}

	b.WriteString("\n---\n\n")
	b.WriteString("**Agent-produced — human review required. Do not merge without review.**\n")
	if info.PlaneTask != "" {
		b.WriteString("\n" + info.PlaneTask + "\n")
	}
	return b.String()
}
