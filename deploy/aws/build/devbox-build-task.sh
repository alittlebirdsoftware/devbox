#!/usr/bin/env bash
# devbox-build-task <submit <s3://…/spec.json> | status <taskId> | finalize <taskId> | cancel <taskId>>
#
# The build host's end of the plane scheduler (ticketed build spec, TB-05; D-13):
# the plane runs these verbs through SSM Run Command and reads the LAST line of
# stdout as JSON. Everything larger than a line goes through the tenant's
# artifact bucket, which this host's instance role already writes: the
# submission spec comes from it, the task's artifacts go back to it under
# <taskId>/. Nothing here polls the plane and nothing here holds a plane
# credential; the daemon alone talks to the repository host.
#
#   submit   fetch the spec {ref, repo, agent, task, scope, activeSet, model, budgetUsd, maxTurns},
#            submit it to the daemon under its ref (a repeat resolves to the existing task)
#            → {"taskId":"t…","existing":false}
#   status   the daemon's one-line view of the task (agent-task status --json)
#            → {"id","state","source","ref","prUrl","createdAt","lastEventAt","error"} | {"notFound":true}
#   finalize upload the task's staging directory (out/: plane-task.json, checks.json, diff.patch,
#            transcript, result artifacts) plus status.json to s3://<bucket>/<taskId>/, with bounded
#            retries; the verdict is recorded on the plane only after this succeeds (the staging copy
#            stays on the host either way)            → {"uploaded":true,"files":[…]}
#   cancel   ask the daemon to cancel the task (a task the plane has given up on)
#            → {"cancelled":true}
#
# Runs as root (SSM). Reads the host's facts from /etc/devbox-build.env (build-bootstrap.sh).
set -uo pipefail
. /etc/devbox-build.env
VERB="${1:?submit|status|finalize|cancel}"; ARG="${2:?argument}"
OPERATOR="${OPERATOR:-ubuntu}"
STATE_DIR=/var/lib/devbox-build-task
UPLOAD_ATTEMPTS="${UPLOAD_ATTEMPTS:-5}"
install -d -m 0750 -o root -g "$OPERATOR" "$STATE_DIR"

agent_task() { sudo -u "$OPERATOR" agent-task "$@"; }
fail() { echo "$*" >&2; exit 1; }

case "$VERB" in
  submit)
    [[ "$ARG" == s3://* ]] || fail "submit takes an s3:// URI of the submission spec"
    spec="$STATE_DIR/$(basename "$ARG")"
    aws s3 cp --quiet "$ARG" "$spec" || fail "cannot fetch $ARG"
    ref=$(jq -r '.ref // empty' "$spec"); [ -n "$ref" ] || fail "spec has no ref"
    [[ "$ref" =~ ^[A-Za-z0-9._-]{1,200}$ ]] || fail "spec ref $ref is not host-safe"
    active="$STATE_DIR/${ref}.active.json"
    jq -c '.activeSet // {"active": []}' "$spec" > "$active" && chmod 0644 "$active"
    args=(--repo "$(jq -r '.repo // empty' "$spec")" --agent "$(jq -r '.agent // "claude"' "$spec")" --task "$(jq -r '.task' "$spec")" --ref "$ref" --json)
    s=$(jq -r '.scope // empty' "$spec"); [ -n "$s" ] && args+=(--scope "$s" --active-file "$active")
    b=$(jq -r '.budgetUsd // empty' "$spec"); [ -n "$b" ] && args+=(--budget-usd "$b")
    t=$(jq -r '.maxTurns // empty' "$spec"); [ -n "$t" ] && args+=(--max-turns "$t")
    m=$(jq -r '.model // empty' "$spec"); [ -n "$m" ] && args+=(--model "$m")
    out=$(agent_task submit "${args[@]}" 2>&1) || fail "submit refused: $out"
    # the daemon's {"taskId","existing"} line is the answer
    tail -n 1 <<<"$out"
    ;;
  status)
    out=$(agent_task status --json "$ARG" 2>&1); rc=$?
    if (( rc != 0 )); then
      grep -qi "not found" <<<"$out" && { echo '{"notFound":true}'; exit 0; }
      fail "status failed: $out"
    fi
    tail -n 1 <<<"$out"
    ;;
  finalize)
    out="/var/lib/agent-work/$ARG/out"
    [ -d "$out" ] || fail "no staging directory for $ARG at $out"
    # the daemon's view of the task rides along, so the plane can read everything from the bucket
    agent_task status --json "$ARG" > "$out/status.json" 2>/dev/null || true
    for (( i=1; i<=UPLOAD_ATTEMPTS; i++ )); do
      if aws s3 cp --quiet --recursive "$out" "s3://${ARTIFACT_BUCKET}/${ARG}/"; then
        files=$(cd "$out" && find . -maxdepth 1 -type f | sed 's#^\./##' | sort | jq -R . | jq -sc .)
        echo "{\"uploaded\":true,\"files\":${files}}"
        exit 0
      fi
      sleep $(( i * 5 ))
    done
    fail "artifact upload failed $UPLOAD_ATTEMPTS times; staging kept at $out"
    ;;
  cancel)
    out=$(agent_task cancel "$ARG" 2>&1) || { grep -qi "not running" <<<"$out" && { echo '{"cancelled":false,"reason":"not running"}'; exit 0; }; fail "cancel failed: $out"; }
    echo '{"cancelled":true}'
    ;;
  *) fail "unknown verb $VERB" ;;
esac
