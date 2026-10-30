#!/bin/sh
# Run only inside a disposable Linux cgroup-v2 test environment with delegation
# or root privileges. Empty cgroups remain so memory.events/peak can be audited.
set -eu
: "${CREST_PERF_BINARY:?Set CREST_PERF_BINARY to the absolute crest executable path}"
cg="${CREST_PERF_CGROUP_PREFIX:-/sys/fs/cgroup/crest-perf}-$$"
mkdir "$cg"
printf '%s\n' "${CREST_PERF_MEMORY_MAX_BYTES:-268435456}" > "$cg/memory.max"
printf '0\n' > "$cg/memory.swap.max"
printf '%s\n' "$$" > "$cg/cgroup.procs"
exec "$CREST_PERF_BINARY" "$@"
