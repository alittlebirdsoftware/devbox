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
exec 9>"$LOCK"; flock -n 9 || { echo "bridge already running"; exit 0; }

open_agent_prs=$(gh pr list --repo "$REPO" --state open --json headRefName --jq '[.[] | select(.headRefName | startswith("agent/"))] | length')
if (( open_agent_prs >= MAX_OPEN_PRS )); then
  echo "backpressure: $open_agent_prs open agent PRs >= $MAX_OPEN_PRS; not starting new tasks"; exit 0
fi

mapfile -t issues < <(gh issue list --repo "$REPO" --state open --label agent --json number,labels \
  --jq '.[] | select([.labels[].name] | index("agent:running") or index("agent:done") or index("agent:failed") | not) | .number' | head -n "$MAX_PER_TICK")
(( ${#issues[@]} )) || { echo "no eligible issues"; exit 0; }

for n in "${issues[@]}"; do
  echo "== issue #$n"
  gh issue edit "$n" --repo "$REPO" --add-label agent:running >/dev/null
  set +e
  out=$(agent-task run --repo "$REPO_NAME" --issue "$n" --agent "$AGENT" --auth "$AUTH" 2>&1); rc=$?
  set -e
  echo "$out" | tail -8
  if (( rc == 0 )) && grep -q '^pr:' <<<"$out"; then
    gh issue edit "$n" --repo "$REPO" --remove-label agent:running --add-label agent:done >/dev/null
  elif grep -qiE 'usage limit|rate limit|limit reached|try again (later|at)|overloaded' <<<"$out"; then
    # The subscription window is exhausted (or the API is throttling). Not the issue's fault:
    # put it back in the queue and stop this tick so the rest of the backlog waits too.
    gh issue edit "$n" --repo "$REPO" --remove-label agent:running >/dev/null
    echo "model usage limit hit; issue #$n returned to the queue, stopping this tick"; exit 0
  else
    gh issue edit "$n" --repo "$REPO" --remove-label agent:running --add-label agent:failed >/dev/null
    gh issue comment "$n" --repo "$REPO" --body "devbox run failed (exit $rc). Last lines:
\`\`\`
$(echo "$out" | tail -12)
\`\`\`
Relabel \`agent\` to retry." >/dev/null
  fi
done
