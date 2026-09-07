# Devbox on EC2 (the plane's copy)

`bootstrap.sh` turns a fresh Ubuntu 22.04 EC2 instance into the devbox host described in
`docs/runbook/0001-devbox-vm.md`: rootless Podman under `agentbox`, the uid egress deny-list,
the `agent-taskd` daemon with `LoadCredential` secrets, the agent base image, and the bridge timer.
It is idempotent and takes no secrets as input: every credential is read from AWS Secrets Manager
under one prefix through the instance role. The CloudFormation template that launches the instance
lives in the controlplane repo (`infra/devbox/template.yaml`) and pins this script by commit.

Secrets expected under the prefix (default `plane/devbox/`), one plain-string secret each:

| Secret name | Becomes `LoadCredential` | Content |
|---|---|---|
| `plane/devbox/gh-token-<repo>` | `gh-token-<repo>` | fine-grained GitHub token scoped to that repo (contents rw, PRs rw, issues rw) |
| `plane/devbox/claude-oauth-token` | `claude-oauth-token` | `claude setup-token` from the dedicated devbox subscription account |
| `plane/devbox/claude-mcp-credentials` | `claude-mcp-credentials` | the `mcpOAuth` section of a Linux `~/.claude/.credentials.json` after `claude` authenticated the Artlist MCP once |

Access: no SSH ingress. Use Session Manager (`aws ssm start-session --target <instance-id>`), then
`sudo -iu ubuntu agent-task status`. Logs: `journalctl -u agent-taskd`, `journalctl -u devbox-bridge`,
`/var/log/devbox-base-build.log`, `/var/log/cloud-init-output.log` for the bootstrap itself.
