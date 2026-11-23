#!/usr/bin/env bash
# Times what an agent hook and a short q command cost as whole processes, which
# the Go benchmarks cannot see: exec, dynamic loading, package init and any
# endpoint security scanner that inspects each new process.
#
#   scripts/bench-exec.sh [path/to/q] [runs]
#
# The hook runs against an empty state directory, so it spools instead of
# reaching a daemon and never touches the real board. `q daemon status` reads
# the real daemon's handle and asks it for its health, which changes nothing.
set -euo pipefail

q=${1:-$(command -v q)}
runs=${2:-50}

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

# mean_ms runs a command $runs times and prints the mean wall time in ms.
mean_ms() {
	local TIMEFORMAT=%R total
	total=$( { time (for ((i = 0; i < runs; i++)); do "$@" >/dev/null 2>&1 || true; done); } 2>&1)
	awk -v t="$total" -v n="$runs" 'BEGIN { printf "%8.2f ms\n", t * 1000 / n }'
}

echo "q:    $q ($("$q" --version 2>/dev/null))"
echo "size: $(du -h "$q" | cut -f1)"
echo "runs: $runs"
echo

printf '%-38s' "true (process start floor)"
mean_ms /usr/bin/true

printf '%-38s' "q --version"
mean_ms "$q" --version

printf '%-38s' "q hook (no daemon, spools)"
hook() {
	echo '{"session_id":"bench","tool_name":"Bash"}' |
		XDG_STATE_HOME="$scratch/state" XDG_DATA_HOME="$scratch/data" XDG_CONFIG_HOME="$scratch/config" \
			Q_MISSION_ID=ms_000000000000 "$q" hook claude PreToolUse
}
mean_ms hook

printf '%-38s' "q daemon status (real daemon)"
mean_ms "$q" daemon status
