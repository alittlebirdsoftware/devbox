package controller

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Verdict is a task's terminal result under result_contract v1. It is derived
// in exactly one place (BuildVerdict) with a fixed precedence; nothing else
// decides whether a v1 task opens a PR.
type Verdict string

const (
	VerdictCancelled      Verdict = "cancelled"
	VerdictTimedOut       Verdict = "timed_out"
	VerdictBudgetExceeded Verdict = "budget_exceeded"
	VerdictFailed         Verdict = "failed"
	VerdictBlocked        Verdict = "blocked"
	VerdictReplan         Verdict = "replan"
	VerdictChange         Verdict = "change"
)

// Result artifacts the agent may write instead of a change, and the check's outputs.
const (
	replanFile    = "replan.json"
	blockedFile   = "blocked.json"
	checksFile    = "checks.json"
	checkExitFile = "check.exit"
)

// resultFile is one agent-written result artifact, as the host found it.
type resultFile struct {
	Present bool
	Valid   bool // a JSON object with a non-empty string "reason"
}

// checkResult is the wrapper's check run, as the host found it.
type checkResult struct {
	Ran   bool   // check.exit exists (the wrapper ran the check)
	Exit  int    // the check command's exit status
	OK    bool   // checks.json parsed and says ok: true
	Scope string // checks.json's scope
}

// VerdictInput is everything the verdict depends on, all host-observed.
type VerdictInput struct {
	Ended      string // "", "cancelled", "timeout", "shutdown": how the run context ended, if it did
	BudgetStop bool   // the agent's transcript says a budget or turn stop fired
	ExitCode   int
	Commits    int
	Scope      string // the scope the task was submitted with
	HasCheck   bool   // the repo configures a check
	Replan     resultFile
	Blocked    resultFile
	Check      checkResult
}

// BuildVerdict derives the v1 verdict. Precedence, highest first: cancelled;
// timed_out; budget_exceeded (whatever else happened); failed (non-zero agent
// exit, more than one result artifact, an invalid one, or a result artifact
// alongside commits); blocked; replan; change (exit 0, ≥1 commit, and — when
// the repo has a check — the check ran, exited 0, and checks.json says ok for
// the submitted scope); else failed. The reason explains any non-change verdict.
func BuildVerdict(in VerdictInput) (Verdict, string) {
	switch in.Ended {
	case "cancelled":
		return VerdictCancelled, "cancelled"
	case "timeout":
		return VerdictTimedOut, "timeout"
	case "shutdown":
		return VerdictFailed, "interrupted by daemon shutdown"
	}
	if in.BudgetStop {
		return VerdictBudgetExceeded, "the agent's budget stop fired"
	}
	if in.ExitCode != 0 {
		return VerdictFailed, fmt.Sprintf("agent exited %d", in.ExitCode)
	}
	results := 0
	for _, r := range []resultFile{in.Replan, in.Blocked} {
		if r.Present {
			results++
			if !r.Valid {
				return VerdictFailed, "invalid result artifact (want a JSON object with a reason)"
			}
		}
	}
	switch {
	case results > 1:
		return VerdictFailed, "more than one result artifact"
	case results == 1 && in.Commits > 0:
		return VerdictFailed, "a result artifact alongside commits"
	case in.Blocked.Present:
		return VerdictBlocked, "blocked"
	case in.Replan.Present:
		return VerdictReplan, "replan"
	case in.Commits < 1:
		return VerdictFailed, "no commits"
	case !in.HasCheck:
		return VerdictChange, ""
	case !in.Check.Ran:
		return VerdictFailed, "check did not run"
	case in.Check.Exit != 0 || !in.Check.OK:
		return VerdictFailed, "checks failed"
	case in.Check.Scope != in.Scope:
		return VerdictFailed, fmt.Sprintf("checks ran scope %q, task scope is %q", in.Check.Scope, in.Scope)
	}
	return VerdictChange, ""
}

// readVerdictInputs fills the artifact-derived fields of a VerdictInput from
// the out dir. Only regular files are read (a symlinked artifact is absent).
func readVerdictInputs(outDir string, in *VerdictInput) {
	in.Replan = readResultFile(filepath.Join(outDir, replanFile))
	in.Blocked = readResultFile(filepath.Join(outDir, blockedFile))
	if s := readArtifact(outDir, checkExitFile); s != "" {
		in.Check.Ran = true
		n, err := strconv.Atoi(s)
		if err != nil {
			n = -1
		}
		in.Check.Exit = n
		var c struct {
			OK    bool   `json:"ok"`
			Scope string `json:"scope"`
		}
		if b := readArtifact(outDir, checksFile); b != "" && json.Unmarshal([]byte(b), &c) == nil {
			in.Check.OK, in.Check.Scope = c.OK, c.Scope
		}
	}
	in.BudgetStop = transcriptBudgetStop(outDir)
}

func readResultFile(path string) resultFile {
	fi, err := os.Lstat(path)
	if err != nil {
		return resultFile{}
	}
	if !fi.Mode().IsRegular() {
		return resultFile{Present: true}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return resultFile{Present: true}
	}
	var v struct {
		Reason string `json:"reason"`
	}
	valid := json.Unmarshal(b, &v) == nil && strings.TrimSpace(v.Reason) != ""
	return resultFile{Present: true, Valid: valid}
}

// transcriptBudgetStop reads the Claude transcript's result subtype. Claude
// Code ends a print-mode run whose --max-budget-usd or --max-turns stop fired
// with subtype error_max_budget_usd or error_max_turns. Other adapters have no
// budget stop (agent.SupportsBudget), so their transcripts never match.
func transcriptBudgetStop(outDir string) bool {
	b := readArtifact(outDir, "transcript.json")
	if b == "" {
		return false
	}
	var tr struct {
		Subtype string `json:"subtype"`
	}
	if json.Unmarshal([]byte(b), &tr) != nil {
		return false
	}
	return tr.Subtype == "error_max_budget_usd" || tr.Subtype == "error_max_turns"
}
