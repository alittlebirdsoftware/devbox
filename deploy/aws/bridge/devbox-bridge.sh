#!/usr/bin/env bash
# devbox bridge (rung 3): runs on the devbox VM from a systemd timer. Turns labelled
# GitHub issues into agent-task runs and keeps the loop bounded.
#   agent           -> eligible (human or triage put it there)
#   agent:running   -> a task is in flight for it
#   agent:done      -> a PR was opened
#   agent:failed    -> the run failed; a human decides whether to relabel `agent`
# The VM holds a repo-scoped GitHub token (host-only, D3) and NO AWS credentials:
# the Task facts ride in the PR body and the gate workflow writes the ledger row.
set -euo pipefail
REPO="${BRIDGE_REPO:?org/repo}"                 # e.g. alittlebirdsoftware/family-law-backend
REPO_NAME="${BRIDGE_REPO_NAME:?registry name}"  # e.g. family-law-backend (devbox config.yaml)
MAX_OPEN_PRS="${BRIDGE_MAX_OPEN_PRS:-5}"        # policy.tenantDefaults.loop.maxOpenAgentPrs
MAX_PER_TICK="${BRIDGE_MAX_PER_TICK:-1}"
AGENT="${BRIDGE_AGENT:-claude}"
AUTH="${BRIDGE_AUTH:-subscription}"             # subscription: a dedicated Max account's setup-token (flat cost, window-throttled); api_key: metered + workspace cap
MAX_TURNS="${BRIDGE_MAX_TURNS:-}"               # optional ceiling passed to the agent (policy.task.maxIterations)
LOCK=/run/lock/devbox-bridge.lock
# The mcp-persist timer takes this lock briefly to restart the daemon; wait for it rather than drop a tick.
exec 9>"$LOCK"; flock -w 90 9 || { echo "bridge already running"; exit 0; }

# Persist the last run's refreshed MCP store BEFORE starting anything (synchronous; the timer is the
# backup). A task seeded with a store whose refresh token an earlier task already rotated trips the
# provider's reuse detection and revokes the whole grant — a stale store is worse than no task.
if ! sudo -n /usr/local/sbin/devbox-mcp-persist.sh --idle; then echo "mcp persister failed; not starting tasks"; exit 1; fi

open_agent_prs=$(gh pr list --repo "$REPO" --state open --json headRefName --jq '[.[] | select(.headRefName | startswith("agent/"))] | length')
if (( open_agent_prs >= MAX_OPEN_PRS )); then
  echo "backpressure: $open_agent_prs open agent PRs >= $MAX_OPEN_PRS; not starting new tasks"; exit 0
fi

mapfile -t issues < <(gh issue list --repo "$REPO" --state open --label agent --json number,labels \
  --jq '.[] | select([.labels[].name] | index("agent:running") or index("agent:done") or index("agent:failed") | not) | .number' | head -n "$MAX_PER_TICK")
(( ${#issues[@]} )) || { echo "no eligible issues"; exit 0; }

TASK_TIMEOUT_S="${BRIDGE_TASK_TIMEOUT_S:-2400}"   # a hair above devbox's 30m task_timeout

for n in "${issues[@]}"; do
  echo "== issue #$n"
  gh issue edit "$n" --repo "$REPO" --add-label agent:running >/dev/null
  set +e
  # On a daemon-managed host tasks go through the socket: submit, then poll status.
  sub=$(agent-task submit --repo "$REPO_NAME" --issue "$n" --agent "$AGENT" 2>&1); rc=$?
  tid=$(grep -oE 't[0-9]+-[0-9a-f]+' <<<"$sub" | head -1)
  if (( rc != 0 )) || [[ -z "$tid" ]]; then
    set -e
    gh issue edit "$n" --repo "$REPO" --remove-label agent:running --add-label agent:failed >/dev/null
    gh issue comment "$n" --repo "$REPO" --body "devbox submit failed (exit $rc): $(tail -3 <<<"$sub")" >/dev/null
    continue
  fi
  echo "task $tid"
  state=""; out=""
  for (( t=0; t<TASK_TIMEOUT_S; t+=20 )); do
    out=$(agent-task status "$tid" 2>&1)
    state=$(sed -n 's/^state: *//p' <<<"$out" | head -1)
    case "$state" in Completed|Failed|Cancelled) break;; esac
    sleep 20
  done
  set -e
  echo "$out" | tail -8
  # The refreshed MCP OAuth store the container handed back is persisted by the devbox-mcp-persist
  # timer (root), which restarts the daemon only while this lock is free. Nothing to do here.
  if [[ "$state" == "Completed" ]]; then
    gh issue edit "$n" --repo "$REPO" --remove-label agent:running --add-label agent:done >/dev/null
  elif grep -q 'interrupted by daemon shutdown' <<<"$out"; then
    # Host plumbing, not the issue: the daemon restarted under the task. Back to the queue for the next tick.
    gh issue edit "$n" --repo "$REPO" --remove-label agent:running >/dev/null
    echo "task $tid interrupted by a daemon restart; issue #$n returned to the queue"
  elif grep -qiE 'usage limit|rate limit|limit reached|try again (later|at)|overloaded' <<<"$out"; then
    # The subscription window is exhausted (or the API is throttling). Not the issue's fault:
    # put it back in the queue and stop this tick so the rest of the backlog waits too.
    gh issue edit "$n" --repo "$REPO" --remove-label agent:running >/dev/null
    echo "model usage limit hit; issue #$n returned to the queue, stopping this tick"; exit 0
  else
    # devbox marks a run Completed only with >=1 commit, so "no change" lands here too.
    gh issue edit "$n" --repo "$REPO" --remove-label agent:running --add-label agent:failed >/dev/null
    gh issue comment "$n" --repo "$REPO" --body "devbox task \`$tid\` ended ${state:-without a terminal state} (no PR). Last events:
\`\`\`
$(echo "$out" | tail -12)
\`\`\`
Relabel \`agent\` to retry." >/dev/null
  fi
done
