#!/usr/bin/env bash
# Build-lane devbox host bootstrap (ticketed build spec, TB-01). One host per
# active build: one tenant repository, the project type's build-task image
# pulled from ECR, result contract v1 with the project type's check, and no
# bridge, tool server or MCP grant. The job lane's bootstrap (../bootstrap.sh)
# is deliberately separate and untouched: an existing host is never affected by
# a change here.
#
# Run as root from cloud-init user-data:
#   build-bootstrap.sh <devbox-repo> <devbox-ref> <secrets-prefix> <repo owner/name> <repo-name> \
#                      <image-uri> <artifact-bucket> <task-cpus> <task-memory-mb> <task-timeout> <check> <model> \
#                      [repo-host (gitlab|github, default gitlab)] [api-url]
# <repo owner/name> is the repository's path on its host: a GitLab group path (subgroups allowed) or a
# GitHub owner, then the name; it is split at the LAST slash.
# Secrets under <secrets-prefix> (e.g. plane/devbox-build/<tenant>/): repo-token-<repo-name>, claude-oauth-token.
set -euo pipefail
DEVBOX_REPO="${1:?owner/repo}"; DEVBOX_REF="${2:?git ref or sha}"
SECRETS_PREFIX="${3:?secrets manager prefix, e.g. plane/devbox-build/<tenant>/}"
TENANT_REPO="${4:?owner/repo of the tenant repository}"; REPO_NAME="${5:?registry name in config.yaml}"
IMAGE_URI="${6:?build-task image in ECR, repo:tag}"; ARTIFACT_BUCKET="${7:?artifact bucket}"
TASK_CPUS="${8:-2}"; TASK_MEMORY_MB="${9:-4096}"; TASK_TIMEOUT="${10:-30m}"
CHECK="${11:-project-check}"; MODEL="${12:-}"
REPO_HOST="${13:-gitlab}"; REPO_API_URL="${14:-}"
TENANT="$(basename "${SECRETS_PREFIX%/}")"
case "$REPO_HOST" in gitlab|github) ;; *) echo "repo-host must be gitlab or github, got $REPO_HOST" >&2; exit 1 ;; esac
OPERATOR=ubuntu
GOVER=go1.26.8
LOCAL_IMAGE=localhost/devbox-build-task:current
log() { echo "[build-bootstrap] $*"; }

case "$(dpkg --print-architecture)" in
  amd64) ARCH=amd64 ;;
  arm64) ARCH=arm64 ;;
  *) echo "unsupported architecture" >&2; exit 1 ;;
esac

# ---- region from IMDSv2 ---------------------------------------------------------
TOKEN=$(curl -sS -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 300')
REGION=$(curl -sS -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/dynamic/instance-identity/document | jq -r .region)
export AWS_DEFAULT_REGION="$REGION"

# ---- packages ---------------------------------------------------------------------
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq podman slirp4netns uidmap fuse-overlayfs iptables git jq curl unzip python3 >/dev/null
if [ "$REPO_HOST" = github ] && ! command -v gh >/dev/null; then
  # GitHub only: the daemon opens pull requests through gh. GitLab goes through the REST API.
  curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg
  echo "deb [arch=${ARCH} signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list
  apt-get update -qq && apt-get install -y -qq gh >/dev/null
fi
if ! command -v aws >/dev/null; then
  # awscli v2 (the apt awscli is v1 and absent on arm64 jammy).
  curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-$(uname -m).zip" -o /tmp/awscliv2.zip
  (cd /tmp && unzip -qo awscliv2.zip && ./aws/install --update >/dev/null && rm -rf aws awscliv2.zip)
fi

# ---- Go toolchain (pinned, checksum-verified against go.dev) ----------------------
if [ ! -x /usr/local/go/bin/go ] || ! /usr/local/go/bin/go version | grep -q "$GOVER "; then
  cd /tmp && curl -fsSLO "https://go.dev/dl/${GOVER}.linux-${ARCH}.tar.gz"
  EXPECT=$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all' | jq -r --arg f "${GOVER}.linux-${ARCH}.tar.gz" '.[].files[]|select(.filename==$f)|.sha256')
  echo "${EXPECT}  ${GOVER}.linux-${ARCH}.tar.gz" | sha256sum -c -
  rm -rf /usr/local/go && tar -C /usr/local -xzf "${GOVER}.linux-${ARCH}.tar.gz"
fi
echo 'export PATH=/usr/local/go/bin:$PATH' > /etc/profile.d/go.sh

# ---- users: agentbox (container runner), agent-taskd (daemon), agentwork (handoff) --
sysctl -w kernel.unprivileged_userns_clone=1 >/dev/null 2>&1 || true
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

# cgroup v2: delegate cpu/memory/pids to user managers so --cpus/--memory/--pids-limit work rootless
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
SUDO
chmod 0440 /etc/sudoers.d/agent-task-podman && visudo -cf /etc/sudoers.d/agent-task-podman

# ---- secrets: only this build's prefix -> root-owned 0600 files for LoadCredential --------
install -d -m 0700 -o root -g root /etc/agent-task/credentials
CRED_NAMES=""
for arn in $(aws secretsmanager list-secrets --filters Key=name,Values="${SECRETS_PREFIX}" --query 'SecretList[].Name' --output text); do
  case "$arn" in "${SECRETS_PREFIX}"*) ;; *) continue ;; esac   # the filter is a prefix match on words; be exact
  name="${arn##*/}"
  aws secretsmanager get-secret-value --secret-id "$arn" --query SecretString --output text | tr -d '\n' > "/etc/agent-task/credentials/${name}.tmp"
  chmod 0600 "/etc/agent-task/credentials/${name}.tmp" && mv "/etc/agent-task/credentials/${name}.tmp" "/etc/agent-task/credentials/${name}"
  CRED_NAMES="${CRED_NAMES} ${name}"
done
log "credentials staged:${CRED_NAMES}"
for need in "repo-token-${REPO_NAME}" claude-oauth-token; do
  [ -s "/etc/agent-task/credentials/${need}" ] || { echo "missing secret ${SECRETS_PREFIX}${need}" >&2; exit 1; }
done

# ---- devbox: clone the fork at the pinned ref, build, install ---------------------------
install -d -m 0755 -o "$OPERATOR" -g "$OPERATOR" /opt/devbox
if [ ! -d /opt/devbox/.git ]; then sudo -u "$OPERATOR" git clone -q "https://github.com/${DEVBOX_REPO}.git" /opt/devbox; fi
sudo -u "$OPERATOR" git -C /opt/devbox fetch -q origin && sudo -u "$OPERATOR" git -C /opt/devbox checkout -q "$DEVBOX_REF"
sudo -u "$OPERATOR" env PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod HOME=/home/$OPERATOR \
  sh -c 'cd /opt/devbox && go build -o /tmp/agent-task ./cmd/agent-task'
install -m 0755 -o root -g root /tmp/agent-task /usr/local/bin/agent-task

# unit: repo copy + one LoadCredential line per staged secret, inside [Service]
python3 - "$CRED_NAMES" <<'PY'
import sys
names = sys.argv[1].split()
src = open('/opt/devbox/deploy/systemd/agent-taskd.service').read()
lines = [l for l in src.splitlines() if not l.startswith(('LoadCredential=', '#LoadCredential='))]
text = '\n'.join(lines)
head, _, tail = text.partition('[Install]')
creds = '\n'.join(f'LoadCredential={n}:/etc/agent-task/credentials/{n}' for n in names)
open('/etc/systemd/system/agent-taskd.service', 'w').write(head.rstrip() + '\n' + creds + '\n\n[Install]' + tail + '\n')
PY

# ---- the project type's build-task image: pulled, never built here -------------------------
REGISTRY="${IMAGE_URI%%/*}"
aws ecr get-login-password --region "$REGION" | sudo -u agentbox /usr/local/sbin/agentbox-podman login --username AWS --password-stdin "$REGISTRY" >/dev/null
sudo -u agentbox /usr/local/sbin/agentbox-podman pull -q "$IMAGE_URI" >/dev/null
sudo -u agentbox /usr/local/sbin/agentbox-podman tag "$IMAGE_URI" "$LOCAL_IMAGE"
IMAGE_DIGEST=$(sudo -u agentbox /usr/local/sbin/agentbox-podman image inspect "$IMAGE_URI" --format '{{.Digest}}')
log "build-task image ${IMAGE_URI} (${IMAGE_DIGEST}) tagged ${LOCAL_IMAGE}"

# ---- config (no secrets): one repo, result contract v1 ------------------------------------
MODEL_LINE=""; [ -n "$MODEL" ] && MODEL_LINE="    model: ${MODEL}"
API_LINE=""; [ -n "$REPO_API_URL" ] && API_LINE="    api_url: ${REPO_API_URL}"
cat > /etc/agent-task/config.yaml <<CFG
socket_path: /run/agent-task/agent-task.sock
data_dir: /var/lib/agent-task
work_dir: /var/lib/agent-work
image: ${LOCAL_IMAGE}
podman: "sudo -u agentbox /usr/local/sbin/agentbox-podman"
credentials_dir: /var/lib/agent-task-creds
limits:
  max_concurrent: 2
  task_timeout: ${TASK_TIMEOUT}
repos:
  - name: ${REPO_NAME}
    owner: ${TENANT_REPO%/*}
    repo: ${TENANT_REPO##*/}
    host: ${REPO_HOST}
${API_LINE}
    default_branch: main
    token_ref: repo-token-${REPO_NAME}
    image: ${LOCAL_IMAGE}
    limits: { cpus: "${TASK_CPUS}", memory_mb: ${TASK_MEMORY_MB}, task_timeout: ${TASK_TIMEOUT} }
    check: ${CHECK}
    result_contract: v1
${MODEL_LINE}
agents:
  claude:
    auth: subscription
    token_ref: claude-oauth-token
CFG
chmod 0644 /etc/agent-task/config.yaml

# ---- the host's facts, for the harness and the plane ---------------------------------------
cat > /etc/devbox-build.env <<ENV
TENANT=${TENANT}
OPERATOR=${OPERATOR}
REPO_NAME=${REPO_NAME}
TENANT_REPO=${TENANT_REPO}
REPO_HOST=${REPO_HOST}
REPO_API_URL=${REPO_API_URL}
BUSY_PARAMETER=/plane/devbox-build/${TENANT}/busy
ARTIFACT_BUCKET=${ARTIFACT_BUCKET}
IMAGE_URI=${IMAGE_URI}
IMAGE_DIGEST=${IMAGE_DIGEST}
DEVBOX_REF=${DEVBOX_REF}
AWS_DEFAULT_REGION=${REGION}
ENV
chmod 0644 /etc/devbox-build.env
install -m 0755 /opt/devbox/deploy/aws/build/trial-harness.sh /usr/local/bin/devbox-build-trial

# ---- live credentials: the plane rotates the repo token, so devbox reads it at submit time;
# ---- the same refresher reports how busy this host is (the rotator waits for idle hosts) ----
install -m 0755 /opt/devbox/deploy/aws/build/refresh-creds.sh /usr/local/sbin/devbox-build-refresh-creds
cat > /etc/systemd/system/devbox-build-creds.service <<UNIT
[Unit]
Description=refresh the build's live credentials from Secrets Manager
[Service]
Type=oneshot
ExecStart=/usr/local/sbin/devbox-build-refresh-creds ${SECRETS_PREFIX}
UNIT
cat > /etc/systemd/system/devbox-build-creds.timer <<'UNIT'
[Unit]
Description=refresh the build's live credentials every 10 minutes
[Timer]
OnBootSec=1min
OnUnitActiveSec=10min
[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload && /usr/local/sbin/devbox-build-refresh-creds "${SECRETS_PREFIX}"
systemctl enable --now devbox-build-creds.timer

systemctl daemon-reload && systemctl enable --now agent-taskd
log "done: $(sudo -u "$OPERATOR" agent-task repos 2>&1 | head -3 | tr '\n' ' ')"
