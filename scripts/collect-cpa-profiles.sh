#!/usr/bin/env bash
# Collect existing pprof endpoints without changing CPA configuration or forcing GC.
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "Usage: $0 http://127.0.0.1:PORT/debug/pprof" >&2
  exit 2
fi
base=${1%/}
case "$base" in
  http://127.0.0.1:*|http://localhost:*|http://\[::1\]:*) ;;
  *) echo "Use the existing loopback endpoint or an SSH-forwarded loopback port." >&2; exit 2 ;;
esac
out=$(mktemp -d "${TMPDIR:-/tmp}/cpa-profiles-$(date +%Y%m%d-%H%M%S)-XXXXXX")
umask 077
{
  date -u '+started_utc=%Y-%m-%dT%H:%M:%SZ'
  printf 'cpu_seconds=15\nheap_force_gc=false\n'
} > "$out/manifest.txt"
collect() {
  local name=$1 route=$2
  if ! curl --fail --silent --show-error --max-time 45 "$base/$route" -o "$out/$name"; then
    printf 'failed=%s\n' "$name" >> "$out/manifest.txt"
    return 1
  fi
}
collect goroutine.txt 'goroutine?debug=2'
collect mutex.pb.gz 'mutex'
collect heap.pb.gz 'heap?gc=0'
collect cpu.pb.gz 'profile?seconds=15'
date -u '+finished_utc=%Y-%m-%dT%H:%M:%SZ' >> "$out/manifest.txt"
printf 'Profiles saved to %s\n' "$out"
