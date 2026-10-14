#!/usr/bin/env bash
# devbox-build-refresh-creds <secrets-prefix>
# Pull every secret under this build's prefix into the daemon's live credentials
# directory (config credentials_dir), which devbox reads at submit time. The
# plane keeps the repo token in the prefix current (GitHub: a one-hour token
# re-minted every 20 minutes; GitLab: the bot user's token, rotated in place
# when due); this runs every 10 minutes, so a task always starts with the
# current token. Files: owner agent-taskd, 0400, written then renamed so a
# reader never sees a partial token. No daemon restart.
#
# It also publishes how busy this host is (tasks not yet terminal) to the SSM
# parameter in BUSY_PARAMETER: the plane's rotator waits for idle hosts before
# rotating a GitLab token, since rotation revokes the old one at once.
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

# ---- busy flag: Created and Running are the live states (store.ListTasks); the rest are terminal.
if [ -n "${BUSY_PARAMETER:-}" ]; then
  busy=$(sudo -u "${OPERATOR:-ubuntu}" agent-task ls 2>/dev/null | awk -F'\t' '$4 == "Created" || $4 == "Running" { n++ } END { print n + 0 }')
  aws ssm put-parameter --name "$BUSY_PARAMETER" --type String --overwrite --value "${busy:-0}" >/dev/null \
    || echo "busy flag publish failed (${BUSY_PARAMETER})" >&2
fi
