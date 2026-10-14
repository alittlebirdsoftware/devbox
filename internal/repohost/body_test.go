package repohost

import (
	"strings"
	"testing"
)

func TestBuildChangeBody(t *testing.T) {
	body := BuildChangeBody(ChangeInfo{
		TaskID: "t123", Agent: "claude",
		IssueURL: "https://github.com/o/r/issues/7",
		Summary:  "Added subtract().",
	})
	for _, want := range []string{
		"`t123`", "`claude`",
		"https://github.com/o/r/issues/7",
		"Added subtract().",
		"human review required",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
	// The reworded marker must NOT carry the emoji AI-attribution pattern.
	if strings.Contains(body, "🤖") {
		t.Error("PR body contains the 🤖 AI-attribution emoji")
	}
}

func TestBuildChangeBodyNoIssueNoSummary(t *testing.T) {
	body := BuildChangeBody(ChangeInfo{TaskID: "t1", Agent: "mock"})
	if strings.Contains(body, "- Issue:") {
		t.Error("issue line present with no issue URL")
	}
	if !strings.Contains(body, "no summary") {
		t.Error("missing empty-summary placeholder")
	}
}

func TestBuildChangeBodyTestOutput(t *testing.T) {
	body := BuildChangeBody(ChangeInfo{TaskID: "t1", Agent: "claude", TestOutput: "ok 5 tests"})
	if !strings.Contains(body, "## Test results") || !strings.Contains(body, "ok 5 tests") {
		t.Errorf("test output not rendered:\n%s", body)
	}
}

func TestBuildChangeBodyPlaneTask(t *testing.T) {
	body := BuildChangeBody(ChangeInfo{TaskID: "t1", Agent: "claude", PlaneTask: "<!-- plane-task {\"taskId\":\"t1\"} -->"})
	if !strings.Contains(body, "<!-- plane-task {") || !strings.HasSuffix(strings.TrimSpace(body), "-->") {
		t.Errorf("plane-task comment not carried:\n%s", body)
	}
}
