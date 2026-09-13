#!/usr/bin/env bash
# Persist the newest VALID refreshed MCP OAuth store any task handed back, whoever submitted it.
# Rules: only a store that still carries a refresh token is persisted (a failed refresh leaves a
# stripped entry, which must never overwrite a good one — and must not hide an older good one behind
# it); each store is examined once (mtime state). Re-staging the secret restarts the daemon, which
# kills any task in flight ("interrupted by daemon shutdown"): the restart is taken only while the
# bridge lock is free, otherwise deferred to a later minute. This is the ONLY persister.
# Runs as root from a 1-minute timer; env: MCP_CREDS_SECRET, AWS_DEFAULT_REGION.
set -euo pipefail
SECRET="${MCP_CREDS_SECRET:?}"; STATE=/var/lib/agent-task/mcp-persist.mtime
LOCK=/run/lock/devbox-bridge.lock; PENDING=/var/lib/agent-task/mcp-restart.pending
# /run/lock is sticky + world-writable and the lock belongs to the bridge user: with fs.protected_regular
# root may not open it O_CREAT, so open it read-only (flock works on a read descriptor).
[ -e "$LOCK" ] || install -m 0644 -o ubuntu -g ubuntu /dev/null "$LOCK"
exec 8<"$LOCK"
restage_if_idle() {
  if flock -n 8; then
    /usr/local/sbin/devbox-refresh-secrets.sh >/dev/null && rm -f "$PENDING" && echo "daemon re-staged"
    flock -u 8
  else
    touch "$PENDING"; echo "re-stage deferred: the bridge is mid-run"
  fi
}
[ -f "$PENDING" ] && restage_if_idle
last=$(cat "$STATE" 2>/dev/null || echo 0)
mapfile -t files < <(find /var/lib/agent-work -maxdepth 3 -name claude-credentials.json -newermt "@$last" -printf '%T@ %p\n' 2>/dev/null | sort -rn)
(( ${#files[@]} )) || exit 0
newest_mtime=${files[0]%% *}
for entry in "${files[@]}"; do
  file=${entry#* }
  if jq -e '(.mcpOAuth | length > 0) and ([.mcpOAuth[] | has("refreshToken")] | all)' "$file" >/dev/null 2>&1; then
    aws secretsmanager put-secret-value --secret-id "$SECRET" --secret-string "$(jq -c '{mcpOAuth}' "$file")" >/dev/null
    echo "persisted mcp store from $file"
    restage_if_idle
    break
  else
    echo "skipped $file: no refresh token (a failed refresh must not overwrite the good store)"
  fi
done
printf '%s' "${newest_mtime%.*}" > "$STATE"
