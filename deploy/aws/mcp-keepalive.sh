#!/usr/bin/env bash
# Keep the Artlist MCP session warm and detect a dead grant before a real job does.
# Artlist invalidated the session server-side after ~12 h idle (2026-09-14) although the refresh token still
# exchanged. Every few hours: take the bridge lock (never refresh concurrently with a task — rotation reuse
# revokes the grant), start from the freshest store, run a get_balance-only task, persist its refreshed store,
# and if the server still says "Needs authentication", open ONE issue in the tenant repo so a human re-auths.
# Runs as the operator user from a systemd timer; env from /etc/devbox-bridge.env (BRIDGE_REPO, BRIDGE_REPO_NAME).
set -euo pipefail
REPO="${BRIDGE_REPO:?}"; REPO_NAME="${BRIDGE_REPO_NAME:?}"; AGENT="${BRIDGE_AGENT:-claude}"
LOCK=/run/lock/devbox-bridge.lock
exec 9>"$LOCK"; flock -w 600 9 || { echo "keepalive: bridge lock busy for 10 min; skipping"; exit 0; }
sudo -n /usr/local/sbin/devbox-mcp-persist.sh --idle >/dev/null || echo "keepalive: persister failed (continuing)"
# Fail CLOSED: the probe must state success explicitly. Matching failure phrasings does not work — an agent
# wrote "isn't authorized in this session", which no failure pattern caught, so the probe reported ok (2026-09-15).
sub=$(agent-task submit --repo "$REPO_NAME" --agent "$AGENT" --task 'Diagnostic only, no repo changes, do not commit, no other tools: call mcp__artlist__get_balance. Your entire final message must be one line, either "ARTLIST_OK <credits>" if the call returned a balance, or "ARTLIST_FAIL <short reason>" for any other outcome, including the tool being unavailable or unauthorised.' 2>&1)
tid=$(grep -oE 't[0-9]+-[0-9a-f]+' <<<"$sub" | head -1)
[[ -n "$tid" ]] || { echo "keepalive: submit failed: $sub"; exit 1; }
for (( t=0; t<300; t+=15 )); do
  state=$(agent-task status "$tid" 2>&1 | sed -n 's/^state: *//p' | head -1)
  case "$state" in Completed|Failed|Cancelled) break;; esac; sleep 15
done
sudo -n /usr/local/sbin/devbox-mcp-persist.sh --idle >/dev/null || true
summary="/var/lib/agent-work/$tid/out/summary.txt"
if ! sudo -n grep -q 'ARTLIST_OK' "$summary" 2>/dev/null; then
  title="Artlist session needs re-authentication (devbox keepalive)"
  if ! gh issue list --repo "$REPO" --state open --search "\"$title\" in:title" --json number --jq 'length' | grep -qv '^0$'; then
    gh issue create --repo "$REPO" --title "$title" --label plane --body "The devbox keepalive probe ($tid) found the Artlist MCP server unauthorised at $(date -u +%FT%TZ). Hero generations will block until someone re-authenticates on the Mac CLI, exports the store to plane/devbox/claude-mcp-credentials and re-stages the host (see the control-plane memory notes). Close this issue once done." >/dev/null
    echo "keepalive: Artlist UNAUTHORISED — issue opened"
  else
    echo "keepalive: Artlist UNAUTHORISED — issue already open"
  fi
  exit 0
fi
echo "keepalive: Artlist ok ($tid $state) — $(sudo -n grep -o 'ARTLIST_OK.*' "$summary" 2>/dev/null | head -1)"
