package controller

import (
	"fmt"

	"github.com/iQonAi/devbox/internal/runner"
)

// wrapperCmd builds the container command: it sets a git identity, runs the
// agent, then — agent-agnostic — bundles the agent's commits (base..HEAD),
// captures the diff, and exits with the agent's status. `set +e` ensures a
// failing agent still gets its artifacts collected. base is a commit SHA (not
// a secret); the agent command comes from the adapter, and it — knowing its
// own transcript shape — also writes summary.txt (§8.8), so no agent-specific
// parsing lives here.
//
// check, when set, is the repo's check command (operator config, one line, no
// single quotes — config validates it). It runs only when the agent exited 0
// with at least one commit, from the source tree, as
// `<check> <scope> /task/check-active.json <out>/checks.json`; the scope and
// active set arrive by env-file and are written out AFTER the agent exits so
// the agent cannot pre-empt them, and any checks.json the agent left is removed
// first. The check's own exit status goes to check.exit. This is a gate on the
// task's verdict, not evidence for merge: tenant CI re-runs the scope outside
// the container.
func wrapperCmd(base, agentCmd, checkCmd string) []string {
	script := fmt.Sprintf(`set +e
cd %[1]s
exec 2> %[2]s/run.log
# /tmp is a 64 MB tmpfs; toolchains that need more (an image may set TMPDIR=$HOME/tmp) get room on the task volume.
mkdir -p "$HOME/tmp"
git config user.email "agent@localhost"
git config user.name "agent-task"
if [ -n "${%[5]s:-}" ]; then
  umask 077
  printf '%%s' "$%[5]s" > "$HOME/.claude.json"
  unset %[5]s
fi
if [ -n "${%[6]s:-}" ]; then
  umask 077
  mkdir -p "$HOME/.claude"
  printf '%%s' "$%[6]s" > "$HOME/.claude/.credentials.json"
  unset %[6]s
fi
umask 022
%[3]s
AGENT_EXIT=$?
if [ -n '%[7]s' ]; then
  rm -f %[2]s/checks.json %[2]s/check.exit %[2]s/check.log
  if [ "$AGENT_EXIT" -eq 0 ] && [ "$(git rev-list %[4]s..HEAD --count 2>/dev/null || echo 0)" -gt 0 ]; then
    printf '%%s' "$%[8]s" > /task/check-active.json
    ( %[7]s "${%[9]s:-}" /task/check-active.json %[2]s/checks.json ) > %[2]s/check.log 2>&1
    printf '%%s\n' "$?" > %[2]s/check.exit
  fi
fi
# MCP OAuth tokens rotate on refresh: hand the refreshed store back to the host as an artifact
# so the next task starts from it (the host re-seeds its secret; see deploy/aws/bridge).
if [ -f "$HOME/.claude/.credentials.json" ]; then
  # subshell: the 077 must not leak onto the bundle/diff below, which the host daemon reads
  ( umask 077; cp "$HOME/.claude/.credentials.json" %[2]s/claude-credentials.json )
fi
if [ "$(git rev-list %[4]s..HEAD --count 2>/dev/null || echo 0)" -gt 0 ]; then
  git bundle create %[2]s/changes.bundle %[4]s..HEAD
  git diff %[4]s HEAD > %[2]s/diff.patch
fi
printf '%%s\n' "$AGENT_EXIT" > %[2]s/agent.exit
exit "$AGENT_EXIT"
`, runner.SrcPath, runner.OutPath, agentCmd, base, EnvMCPServers, EnvMCPCreds, checkCmd, EnvCheckActive, EnvCheckScope)
	// bash -c, NOT -lc: a login shell sources /etc/profile and ~/.profile, but
	// the container runs with HOME=/task (no profile there), so -l resets PATH
	// and drops /home/agent/.local/bin where the agent CLIs live. -c inherits
	// the image's ENV PATH intact.
	return []string{"bash", "-c", script}
}
