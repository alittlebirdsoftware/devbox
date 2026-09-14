#!/usr/bin/env bash
# Devbox host bootstrap for an EC2 Ubuntu 22.04 instance. Reproduces the hand-built
# Proxmox VM from docs/runbook/0001-devbox-vm.md, idempotently, with secrets pulled
# from AWS Secrets Manager through the instance role (never baked into the image
# or this script). Run as root from cloud-init user-data:
#   bootstrap.sh <devbox-repo> <devbox-ref> <secrets-prefix> <bridge-repo> <bridge-repo-name>
set -euo pipefail
DEVBOX_REPO="${1:?owner/repo}"; DEVBOX_REF="${2:?git ref or sha}"
SECRETS_PREFIX="${3:?secrets manager prefix, e.g. plane/devbox/}"
BRIDGE_REPO="${4:?owner/repo the bridge watches}"; BRIDGE_REPO_NAME="${5:?registry name in config.yaml}"
OPERATOR=ubuntu
GOVER=go1.26.8
log() { echo "[bootstrap] $*"; }

# ---- region from IMDSv2 ---------------------------------------------------------
TOKEN=$(curl -sS -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 300')
REGION=$(curl -sS -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/dynamic/instance-identity/document | jq -r .region)
export AWS_DEFAULT_REGION="$REGION"

# ---- packages ---------------------------------------------------------------------
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq podman slirp4netns uidmap fuse-overlayfs iptables git jq curl unzip awscli >/dev/null
if ! command -v gh >/dev/null; then
  curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg
  echo "deb [arch=amd64 signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list
  apt-get update -qq && apt-get install -y -qq gh >/dev/null
fi

# ---- Go toolchain (pinned, checksum-verified against go.dev) ----------------------
if [ ! -x /usr/local/go/bin/go ] || ! /usr/local/go/bin/go version | grep -q "$GOVER "; then
  cd /tmp && curl -fsSLO "https://go.dev/dl/${GOVER}.linux-amd64.tar.gz"
  EXPECT=$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all' | jq -r --arg f "${GOVER}.linux-amd64.tar.gz" '.[].files[]|select(.filename==$f)|.sha256')
  echo "${EXPECT}  ${GOVER}.linux-amd64.tar.gz" | sha256sum -c -
  rm -rf /usr/local/go && tar -C /usr/local -xzf "${GOVER}.linux-amd64.tar.gz"
fi
echo 'export PATH=/usr/local/go/bin:$PATH' > /etc/profile.d/go.sh

# ---- users: agentbox (container runner), agent-taskd (daemon), agentwork (handoff) --
sysctl -w kernel.unprivileged_userns_clone=1 >/dev/null
echo 'kernel.unprivileged_userns_clone=1' > /etc/sysctl.d/90-devbox.conf
id agentbox >/dev/null 2>&1 || useradd --system --create-home --home-dir /home/agentbox --shell /usr/sbin/nologin --comment "agent container runner" agentbox
id agent-taskd >/dev/null 2>&1 || useradd --system --user-group --home-dir /var/lib/agent-task --no-create-home --shell /usr/sbin/nologin --comment "agent-taskd daemon" agent-taskd
groupadd -f agentwork
usermod -aG agentwork agent-taskd; usermod -aG agentwork agentbox
usermod -aG agent-taskd "$OPERATOR"
AGENTBOX_UID=$(id -u agentbox)
grep -q "^agentbox:" /etc/subuid || echo "agentbox:200000:65536" >> /etc/subuid
grep -q "^agentbox:" /etc/subgid || echo "agentbox:200000:65536" >> /etc/subgid
loginctl enable-linger agentbox
install -d -m 0755 -o agentbox -g agentbox /home/agentbox/.config/containers
cat > /home/agentbox/.config/containers/containers.conf <<'CONF'
[engine]
events_logger = "file"
CONF
chown agentbox:agentbox /home/agentbox/.config/containers/containers.conf
install -d -m 2770 -o agent-taskd -g agentwork /var/lib/agent-work

# cgroup v2: delegate cpu to user managers so --cpus works rootless
mkdir -p /etc/systemd/system/user@.service.d
cat > /etc/systemd/system/user@.service.d/delegate.conf <<'DROP'
[Service]
Delegate=cpu cpuset io memory pids
DROP
systemctl daemon-reload
systemctl restart "user@${AGENTBOX_UID}.service" || true

# ---- egress deny-list for the container-runner uid (RFC1918, CGNAT, link-local/metadata) --
cat > /usr/local/sbin/agentbox-egress.sh <<'EGRESS'
#!/bin/sh
# Idempotent: delete-then-add each rule once. Containers run rootless as agentbox and
# slirp4netns opens their sockets as that uid, so an OUTPUT owner match is the boundary.
UID_=$(id -u agentbox)
for net in 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 169.254.0.0/16; do
  while iptables -D OUTPUT -m owner --uid-owner "$UID_" -d "$net" -j REJECT 2>/dev/null; do :; done
  iptables -A OUTPUT -m owner --uid-owner "$UID_" -d "$net" -j REJECT
done
EGRESS
chmod 0755 /usr/local/sbin/agentbox-egress.sh
cat > /etc/systemd/system/agentbox-egress.service <<'UNIT'
[Unit]
Description=egress deny-list for the agentbox container-runner uid
After=network-online.target
Wants=network-online.target
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/agentbox-egress.sh
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload && systemctl enable --now agentbox-egress.service

# ---- cross-user podman hop: root-owned wrapper + narrow sudoers rule ----------------
cat > /usr/local/sbin/agentbox-podman <<WRAP
#!/bin/sh
export HOME=/home/agentbox
export XDG_RUNTIME_DIR=/run/user/${AGENTBOX_UID}
exec /usr/bin/podman "\$@"
WRAP
chown root:root /usr/local/sbin/agentbox-podman && chmod 0755 /usr/local/sbin/agentbox-podman
cat > /etc/sudoers.d/agent-task-podman <<SUDO
agent-taskd ALL=(agentbox) NOPASSWD: /usr/local/sbin/agentbox-podman
${OPERATOR} ALL=(agentbox) NOPASSWD: /usr/local/sbin/agentbox-podman
${OPERATOR} ALL=(root) NOPASSWD: /usr/local/sbin/devbox-mcp-persist.sh --idle
SUDO
chmod 0440 /etc/sudoers.d/agent-task-podman && visudo -cf /etc/sudoers.d/agent-task-podman

# ---- secrets: Secrets Manager -> root-owned 0600 files for LoadCredential ---------------
install -d -m 0700 -o root -g root /etc/agent-task/credentials
CRED_NAMES=""
for arn in $(aws secretsmanager list-secrets --filters Key=name,Values="${SECRETS_PREFIX}" --query 'SecretList[].Name' --output text); do
  name="${arn##*/}"
  aws secretsmanager get-secret-value --secret-id "$arn" --query SecretString --output text | tr -d '\n' > "/etc/agent-task/credentials/${name}.tmp"
  chmod 0600 "/etc/agent-task/credentials/${name}.tmp" && mv "/etc/agent-task/credentials/${name}.tmp" "/etc/agent-task/credentials/${name}"
  CRED_NAMES="${CRED_NAMES} ${name}"
done
log "credentials staged:${CRED_NAMES}"

# ---- devbox: clone the plane fork at the pinned ref, build, install --------------------
install -d -m 0755 -o "$OPERATOR" -g "$OPERATOR" /opt/devbox
if [ ! -d /opt/devbox/.git ]; then sudo -u "$OPERATOR" git clone -q "https://github.com/${DEVBOX_REPO}.git" /opt/devbox; fi
sudo -u "$OPERATOR" git -C /opt/devbox fetch -q origin && sudo -u "$OPERATOR" git -C /opt/devbox checkout -q "$DEVBOX_REF"
sudo -u "$OPERATOR" env PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod HOME=/home/$OPERATOR \
  sh -c 'cd /opt/devbox && go build -o /tmp/agent-task ./cmd/agent-task'
install -m 0755 -o root -g root /tmp/agent-task /usr/local/bin/agent-task

# unit: repo copy + one LoadCredential line per staged secret
{
  sed -e '/^LoadCredential=/d' -e '/^#LoadCredential=/d' /opt/devbox/deploy/systemd/agent-taskd.service
  for n in $CRED_NAMES; do echo "LoadCredential=${n}:/etc/agent-task/credentials/${n}"; done
} > /etc/systemd/system/agent-taskd.service
# LoadCredential lines must sit in [Service]; move the [Install] block to the end
python3 - <<'PY'
p='/etc/systemd/system/agent-taskd.service'; s=open(p).read()
head, _, tail = s.partition('[Install]')
lines = tail.splitlines(); install=[]; extra=[]
for l in lines:
    (extra if l.startswith('LoadCredential=') else install).append(l)
open(p,'w').write(head + '\n'.join(extra) + '\n\n[Install]' + '\n'.join(install) + '\n')
PY

# config (no secrets): the hero repo with its MCP server, claude on subscription auth
cat > /etc/agent-task/config.yaml <<CFG
socket_path: /run/agent-task/agent-task.sock
data_dir: /var/lib/agent-task
work_dir: /var/lib/agent-work
image: localhost/devbox-agent-base:dev
podman: "sudo -u agentbox /usr/local/sbin/agentbox-podman"
limits:
  max_concurrent: 2
  task_timeout: 30m
repos:
  - name: ${BRIDGE_REPO_NAME}
    owner: ${BRIDGE_REPO%%/*}
    repo: ${BRIDGE_REPO##*/}
    default_branch: main
    token_ref: gh-token-${BRIDGE_REPO_NAME}
    mcp_servers: { artlist: https://mcp.artlist.io/mcp }
    mcp_creds_ref: claude-mcp-credentials
    env_secrets: { GEMINI_API_KEY: gemini-api-key }   # keyed fallback provider for image generation (plane HeroProvider=gemini)
agents:
  claude:
    auth: subscription
    token_ref: claude-oauth-token
CFG
chmod 0644 /etc/agent-task/config.yaml

# ---- agent base image (rootless, as agentbox; ~10 min) ------------------------------------
if ! sudo -u agentbox /usr/local/sbin/agentbox-podman image exists localhost/devbox-agent-base:dev; then
  cp -r /opt/devbox/images/base /tmp/devbox-base && chown -R agentbox:agentbox /tmp/devbox-base
  (cd /tmp/devbox-base && sudo -u agentbox /usr/local/sbin/agentbox-podman build --dns 9.9.9.9 -t devbox-agent-base:dev . >/var/log/devbox-base-build.log 2>&1)
  rm -rf /tmp/devbox-base
fi

# ---- daemon + bridge -------------------------------------------------------------------------
systemctl daemon-reload && systemctl enable --now agent-taskd
install -m 0755 /opt/devbox/deploy/aws/bridge/devbox-bridge.sh /usr/local/bin/devbox-bridge.sh
install -m 0644 /opt/devbox/deploy/aws/bridge/devbox-bridge.service /opt/devbox/deploy/aws/bridge/devbox-bridge.timer /etc/systemd/system/
{
  echo "BRIDGE_REPO=${BRIDGE_REPO}"; echo "BRIDGE_REPO_NAME=${BRIDGE_REPO_NAME}"; echo "BRIDGE_MAX_OPEN_PRS=5"; echo "BRIDGE_AUTH=subscription"
  echo "GH_TOKEN=$(cat /etc/agent-task/credentials/gh-token-${BRIDGE_REPO_NAME})"
  echo "CLAUDE_CODE_OAUTH_TOKEN=$(cat /etc/agent-task/credentials/claude-oauth-token)"
} > /etc/devbox-bridge.env
chown root:"$OPERATOR" /etc/devbox-bridge.env && chmod 0640 /etc/devbox-bridge.env
install -m 0755 /opt/devbox/deploy/aws/mcp-persist.sh /usr/local/sbin/devbox-mcp-persist.sh
install -m 0644 /opt/devbox/deploy/aws/mcp-persist.service /etc/systemd/system/devbox-mcp-persist.service
install -m 0644 /opt/devbox/deploy/aws/mcp-persist.timer /etc/systemd/system/devbox-mcp-persist.timer
install -m 0755 /opt/devbox/deploy/aws/mcp-keepalive.sh /usr/local/sbin/devbox-mcp-keepalive.sh
install -m 0644 /opt/devbox/deploy/aws/mcp-keepalive.service /etc/systemd/system/devbox-mcp-keepalive.service
install -m 0644 /opt/devbox/deploy/aws/mcp-keepalive.timer /etc/systemd/system/devbox-mcp-keepalive.timer
grep -q MCP_CREDS_SECRET /etc/devbox-bridge.env || printf "MCP_CREDS_SECRET=%s\nAWS_DEFAULT_REGION=%s\n" "${SECRETS_PREFIX}claude-mcp-credentials" "$REGION" >> /etc/devbox-bridge.env
systemctl daemon-reload && systemctl enable --now devbox-bridge.timer devbox-mcp-persist.timer devbox-mcp-keepalive.timer
log "done: $(sudo -u "$OPERATOR" agent-task status 2>&1 | head -3 | tr '\n' ' ')"
