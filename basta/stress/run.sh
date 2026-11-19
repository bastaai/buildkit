#!/bin/sh
# Run one driver against a fresh privileged buildkitd container and record the outcome.
# usage: run.sh <image> <label> <out-dir> <driver-binary> [driver flags...]
set -eu
image=$1 label=$2 out=$3 driver=$4; shift 4
name=bk-$label port=${PORT:-12400}
mkdir -p "$out"
docker rm -fv "$name" >/dev/null 2>&1 || true
# Set BUILDKIT_SCHEDULER_DEBUG=1 to opt in; stock panics in debugSchedulerPreUnpark with it.
docker run -d --privileged --name "$name" ${BUILDKIT_SCHEDULER_DEBUG:+-e BUILDKIT_SCHEDULER_DEBUG=$BUILDKIT_SCHEDULER_DEBUG} \
  -p "127.0.0.1:$port:1234" -v /var/lib/buildkit \
  --entrypoint buildkitd "$image" --addr tcp://0.0.0.0:1234 ${BUILDKITD_FLAGS:-} >/dev/null
ready=
for _ in $(seq 1 60); do
  if docker exec "$name" buildctl --addr tcp://127.0.0.1:1234 debug workers >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ -z "$ready" ]; then
  docker logs "$name" > "$out/$label-daemon.log" 2>&1 || true
  docker rm -fv "$name" >/dev/null 2>&1 || true
  echo "harness: daemon $label did not become ready" >&2
  exit 2
fi

rc=0
"$driver" -addr "tcp://127.0.0.1:$port" "$@" > "$out/$label-driver.log" 2>&1 || rc=$?
state=$(docker inspect -f '{{.State.Running}} {{.State.ExitCode}}' "$name")
docker logs "$name" > "$out/$label-daemon.log" 2>&1
docker rm -fv "$name" >/dev/null 2>&1 || true

d=$out/$label-daemon.log
result=$(grep '^RESULT' "$out/$label-driver.log" | tail -1 || true)
{
  echo "label=$label"
  echo "image=$image"
  echo "driver_rc=$rc"
  echo "running=${state% *}"
  echo "exit_code=${state#* }"
  echo "panics=$(grep -c 'panic:' "$d" || true)"
  echo "failed_get_edge=$(grep -c 'failed to get edge' "$d" || true)"
  echo "merged_not_found=$(grep -c 'merged input vertex not found' "$d" || true)"
  echo "released=$(grep -c 'evaluating released result' "$d" || true)"
  echo "result=$result"
} > "$out/$label.env"
cat "$out/$label.env"

# A missing RESULT line with a live daemon means the driver itself broke.
if [ -z "$result" ] && [ "${state% *}" = true ]; then
  echo "harness: driver for $label printed no RESULT (rc=$rc)" >&2
  exit 2
fi
