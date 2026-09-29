#!/usr/bin/env bash
# devbox-build-refresh-creds <secrets-prefix>
# Pull every secret under this build's prefix into the daemon's live credentials
# directory (config credentials_dir), which devbox reads at submit time. The
# plane re-mints the repo-scoped GitHub installation token (one-hour lifetime)
# into the prefix every 20 minutes; this runs every 10, so a task always starts
# with a token that outlives it. Files: owner agent-taskd, 0400, written then
# renamed so a reader never sees a partial token. No daemon restart.
set -euo pipefail
PREFIX="${1:?secrets prefix}"
DIR=/var/lib/agent-task-creds
. /etc/devbox-build.env
install -d -m 0500 -o agent-taskd -g agent-taskd "$DIR"
for arn in $(aws secretsmanager list-secrets --filters Key=name,Values="${PREFIX}" --query 'SecretList[].Name' --output text); do
  case "$arn" in "${PREFIX}"*) ;; *) continue ;; esac
  name="${arn##*/}"
  tmp="$DIR/.${name}.tmp"
  ( umask 377; aws secretsmanager get-secret-value --secret-id "$arn" --query SecretString --output text | tr -d '\n' > "$tmp" )
  [ -s "$tmp" ] || { rm -f "$tmp"; echo "empty secret ${name}; kept the previous value" >&2; continue; }
  chown agent-taskd:agent-taskd "$tmp" && mv -f "$tmp" "$DIR/$name"
done
