#!/usr/bin/env bash
# devbox-build-trial <fixture.json | s3://…/fixture.json> [case …]
#
# The Step 1 substrate trial's stand-in for the plane scheduler (ticketed build
# spec, TB-01 / Step 1). For each fixture case it submits one task with the
# case's scope, active set and budget; samples the task container's memory
# while it runs; waits for the terminal state; uploads the task's artifacts to
# the artifact bucket under <taskId>/ with bounded retries; and only then
# appends the task's verdict to the fixture ledger. A task whose upload keeps
# failing stays "finalizing" in the ledger, never an unrecorded terminal.
#
# Runs as root on a build host (SSM AWS-RunShellScript). Reads the host's facts
# from /etc/devbox-build.env (build-bootstrap.sh). Fixture shape:
#   {"trialId": "step1-…", "cases": [{"name": "…", "task": "…", "scope": "ticket:w1",
#     "activeSet": {"active": [...], "selectors": {...}}, "budgetUsd": 2, "maxTurns": 40,
#     "model": "claude-sonnet-5", "expect": ["change"]}]}
set -uo pipefail
. /etc/devbox-build.env
FIXTURE="${1:?fixture path or s3:// URI}"; shift
ONLY=" $* "
OPERATOR=ubuntu
POLL_S=5
WAIT_S="${WAIT_S:-3600}"
UPLOAD_ATTEMPTS=5
STATE_DIR=/var/lib/devbox-build-trial
install -d -m 0750 -o root -g "$OPERATOR" "$STATE_DIR"

if [[ "$FIXTURE" == s3://* ]]; then
  aws s3 cp --quiet "$FIXTURE" "$STATE_DIR/fixture.json" || { echo "cannot fetch $FIXTURE" >&2; exit 1; }
  FIXTURE="$STATE_DIR/fixture.json"
fi
TRIAL_ID=$(jq -r '.trialId' "$FIXTURE")
LEDGER="$STATE_DIR/${TRIAL_ID}.ledger.jsonl"
LEDGER_URI="s3://${ARTIFACT_BUCKET}/trial/${TRIAL_ID}/ledger.jsonl"
now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# A ledger row is appended locally and the whole ledger re-published; the local
# file is the source of truth if the bucket is briefly unreachable.
ledger() {
  echo "$1" >> "$LEDGER"
  aws s3 cp --quiet "$LEDGER" "$LEDGER_URI" || echo "ledger publish failed (kept locally at $LEDGER)" >&2
}

podman_() { sudo -u agentbox /usr/local/sbin/agentbox-podman "$@"; }

# Peak memory of the task's container (bytes; 0 if never seen), from its
# cgroup: memory.peak where the kernel has it, else the max of memory.current
# sampled every 2 s. Like the local measurements, this includes page cache.
sample_memory() {
  local name="agent-task-$1" peak=0 cur cg=""
  while :; do
    if [ -z "$cg" ]; then
      cg=$(podman_ inspect --format '{{.State.CgroupPath}}' "$name" 2>/dev/null)
      [ -n "$cg" ] && cg="/sys/fs/cgroup${cg}"
    fi
    if [ -n "$cg" ] && [ -d "$cg" ]; then
      for f in memory.peak memory.current; do
        cur=$(cat "$cg/$f" 2>/dev/null)
        if [[ "$cur" =~ ^[0-9]+$ ]] && (( cur > peak )); then peak=$cur; echo "$peak" > "$STATE_DIR/$1.mempeak"; fi
      done
    fi
    sleep 2
  done
}

upload() {
  local id=$1 dir=$2 i
  for (( i=1; i<=UPLOAD_ATTEMPTS; i++ )); do
    if aws s3 cp --quiet --recursive "$dir" "s3://${ARTIFACT_BUCKET}/${id}/"; then return 0; fi
    sleep $(( i * 5 ))
  done
  return 1
}

jq -c '.cases[]' "$FIXTURE" | while read -r c; do
  name=$(jq -r .name <<<"$c")
  [[ "$ONLY" == "  " || "$ONLY" == *" $name "* ]] || continue
  scope=$(jq -r .scope <<<"$c")
  active="$STATE_DIR/${TRIAL_ID}-${name}.active.json"
  jq -c '.activeSet // {"active": []}' <<<"$c" > "$active" && chmod 0644 "$active"
  args=(--repo "$REPO_NAME" --agent claude --task "$(jq -r .task <<<"$c")" --scope "$scope" --active-file "$active")
  b=$(jq -r '.budgetUsd // empty' <<<"$c"); [ -n "$b" ] && args+=(--budget-usd "$b")
  t=$(jq -r '.maxTurns // empty' <<<"$c"); [ -n "$t" ] && args+=(--max-turns "$t")
  m=$(jq -r '.model // empty' <<<"$c"); [ -n "$m" ] && args+=(--model "$m")

  sub=$(sudo -u "$OPERATOR" agent-task submit "${args[@]}" 2>&1)
  if [[ ! "$sub" =~ ^t[0-9]+-[0-9a-f]+$ ]]; then
    echo "[$name] submit refused: $sub"
    ledger "$(jq -nc --arg t "$TRIAL_ID" --arg n "$name" --arg e "$sub" --arg at "$(now)" '{trialId:$t,case:$n,status:"submit_refused",error:$e,at:$at}')"
    continue
  fi
  id=$sub; started=$(now); echo "[$name] task $id ($scope)"
  sample_memory "$id" & sampler=$!

  state=""; status_out=""
  for (( w=0; w<WAIT_S; w+=POLL_S )); do
    status_out=$(sudo -u "$OPERATOR" agent-task status "$id" 2>&1)
    state=$(sed -n 's/^state: *//p' <<<"$status_out" | head -1)
    case "$state" in Completed|Failed|Cancelled) break ;; esac
    sleep "$POLL_S"
  done
  kill "$sampler" 2>/dev/null; wait "$sampler" 2>/dev/null
  peak=$(cat "$STATE_DIR/$id.mempeak" 2>/dev/null || echo 0)

  out="/var/lib/agent-work/$id/out"
  echo "$status_out" > "$out/status.txt" 2>/dev/null || true
  ledger "$(jq -nc --arg t "$TRIAL_ID" --arg n "$name" --arg id "$id" --arg at "$(now)" '{trialId:$t,case:$n,taskId:$id,status:"finalizing",at:$at}')"
  if ! upload "$id" "$out"; then
    echo "[$name] artifact upload failed $UPLOAD_ATTEMPTS times; left finalizing (staging kept at $out)"
    continue
  fi

  facts="{}"; [ -f "$out/plane-task.json" ] && facts=$(jq -c . "$out/plane-task.json")
  checks="null"; [ -f "$out/checks.json" ] && checks=$(jq -c '{ok, scope, passed, failed, undiscovered, analyzer: {errors: .analyzer.errors, warnings: .analyzer.warnings}, suite: {passed: .suite.passed, failed: .suite.failed}, durationMs}' "$out/checks.json")
  verdict=$(jq -r '.buildResult // empty' <<<"$facts")
  [ -z "$verdict" ] && case "$state" in Cancelled) verdict=cancelled ;; *) grep -q '>Failed: timeout' <<<"$status_out" && verdict=timed_out ;; esac
  expect=$(jq -c '.expect // []' <<<"$c")
  met=$(jq -n --arg v "$verdict" --argjson e "$expect" '($e | length) == 0 or ($e | index($v) != null)')
  row=$(jq -nc --arg t "$TRIAL_ID" --arg n "$name" --arg id "$id" --arg scope "$scope" --arg state "$state" \
    --arg v "$verdict" --argjson expect "$expect" --argjson met "$met" --argjson facts "$facts" --argjson checks "$checks" \
    --arg started "$started" --arg at "$(now)" --argjson peak "${peak:-0}" --arg img "$IMAGE_URI" --arg dig "$IMAGE_DIGEST" --arg ref "$DEVBOX_REF" \
    '{trialId:$t, case:$n, taskId:$id, status:"recorded", scope:$scope, state:$state, verdict:$v, expect:$expect, expectMet:$met,
      artifacts:("s3://'"$ARTIFACT_BUCKET"'/" + $id + "/"), startedAt:$started, recordedAt:$at,
      measured:{memoryPeakBytes:$peak, startedAt:$facts.startedAt, finishedAt:$facts.finishedAt, costUsd:$facts.costUsd, iterations:$facts.iterations,
        usage:$facts.usage, model:$facts.agent.model, agentVersion:$facts.agent.version, commits:$facts.commits},
      checks:$checks, image:$img, imageDigest:$dig, devboxRef:$ref}')
  ledger "$row"
  echo "[$name] $state / ${verdict:-?} (expect $expect: $met), peak $(( ${peak:-0} / 1048576 )) MB"
done
echo "ledger: $LEDGER_URI"
