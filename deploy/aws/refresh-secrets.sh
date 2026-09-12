#!/usr/bin/env bash
# Re-pull every secret under the prefix into /etc/agent-task/credentials (root 0600) and
# restart the daemon so LoadCredential picks the new values up. Run as root on the host:
#   refresh-secrets.sh [secrets-prefix=plane/devbox/]
set -euo pipefail
PREFIX="${1:-plane/devbox/}"
TOKEN=$(curl -sS -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')
export AWS_DEFAULT_REGION=$(curl -sS -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/dynamic/instance-identity/document | jq -r .region)
install -d -m 0700 -o root -g root /etc/agent-task/credentials
for arn in $(aws secretsmanager list-secrets --filters Key=name,Values="${PREFIX}" --query 'SecretList[].Name' --output text); do
  name="${arn##*/}"
  aws secretsmanager get-secret-value --secret-id "$arn" --query SecretString --output text | tr -d '\n' > "/etc/agent-task/credentials/${name}.tmp"
  chmod 0600 "/etc/agent-task/credentials/${name}.tmp" && mv "/etc/agent-task/credentials/${name}.tmp" "/etc/agent-task/credentials/${name}"
  echo "refreshed ${name} ($(wc -c < "/etc/agent-task/credentials/${name}") bytes)"
done
systemctl restart agent-taskd && echo "agent-taskd $(systemctl is-active agent-taskd)"
