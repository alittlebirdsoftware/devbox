#!/usr/bin/env bash
# Persist the newest refreshed MCP OAuth store any task handed back, whoever submitted it.
# Rules: only a store that still carries a refresh token is persisted (a failed refresh leaves a
# stripped entry, which must never overwrite a good one); each store is pushed once (mtime state).
# Runs as root from a 1-minute timer; env: MCP_CREDS_SECRET, AWS_DEFAULT_REGION.
set -euo pipefail
SECRET="${MCP_CREDS_SECRET:?}"; STATE=/var/lib/agent-task/mcp-persist.mtime
last=$(cat "$STATE" 2>/dev/null || echo 0)
newest=$(find /var/lib/agent-work -maxdepth 3 -name claude-credentials.json -newermt "@$last" -printf '%T@ %p\n' 2>/dev/null | sort -n | tail -1 || true)
[ -n "$newest" ] || exit 0
mtime=${newest%% *}; file=${newest#* }
if jq -e '(.mcpOAuth | length > 0) and ([.mcpOAuth[] | has("refreshToken")] | all)' "$file" >/dev/null 2>&1; then
  aws secretsmanager put-secret-value --secret-id "$SECRET" --secret-string "$(jq -c '{mcpOAuth}' "$file")" >/dev/null
  /usr/local/sbin/devbox-refresh-secrets.sh >/dev/null
  echo "persisted mcp store from $file"
else
  echo "skipped $file: no refresh token (a failed refresh must not overwrite the good store)"
fi
printf '%s' "${mtime%.*}" > "$STATE"
