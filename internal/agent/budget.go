package agent

import (
	"fmt"
	"strconv"
)

// Budget is a per-task stop the agent enforces on itself: a dollar ceiling and
// an iteration (turn) cap. Zero fields are unset. The scheduler derives both
// from the task's class; the task facts always record what was actually spent,
// read from the transcript.
type Budget struct {
	MaxUSD   float64
	MaxTurns int
}

// IsZero reports whether no stop is set.
func (b Budget) IsZero() bool { return b.MaxUSD <= 0 && b.MaxTurns <= 0 }

// Budgeted is implemented by adapters whose CLI enforces a Budget. A task that
// carries a budget for an adapter without it is refused at submit, never run
// unbounded.
type Budgeted interface {
	Agent
	// BudgetedCommand is Command with the budget's flags applied.
	BudgetedCommand(method AuthMethod, promptPath, transcriptPath string, b Budget) (string, error)
}

// CommandFor returns the adapter's command for a task, applying b when it is set.
func CommandFor(a Agent, method AuthMethod, promptPath, transcriptPath string, b Budget) (string, error) {
	if b.IsZero() {
		return a.Command(method, promptPath, transcriptPath)
	}
	ba, ok := a.(Budgeted)
	if !ok {
		return "", fmt.Errorf("agent %q does not support a budget stop", a.Name())
	}
	return ba.BudgetedCommand(method, promptPath, transcriptPath, b)
}

// SupportsBudget reports whether a can enforce a Budget.
func SupportsBudget(a Agent) bool {
	_, ok := a.(Budgeted)
	return ok
}

func formatUSD(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
