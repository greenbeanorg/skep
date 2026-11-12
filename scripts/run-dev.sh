#!/bin/sh
# Runs skep-agent and skep-api in development mode, as two processes, the
# way they run in production (ADR 0004): the agent on a Unix socket in
# .skep-dev/run/, the API on loopback. Both run as you; the agent's peer
# allow-list is your own UID. Ctrl-C stops both.
#
# This is a development setup on a development machine. It is not a Skep
# host and proves nothing about FreeBSD.
set -eu

cd "$(dirname "$0")/.."
bin="${SKEP_BIN_DIR:-bin}"
for b in skep-agent skep-api; do
	if [ ! -x "${bin}/${b}" ]; then
		echo "run-dev: ${bin}/${b} is missing; run 'make build' first" >&2
		exit 1
	fi
done

agent_pid=""
api_pid=""
cleanup() {
	trap - INT TERM EXIT
	[ -n "${api_pid}" ] && kill "${api_pid}" 2>/dev/null || true
	[ -n "${agent_pid}" ] && kill "${agent_pid}" 2>/dev/null || true
	wait 2>/dev/null || true
}
trap cleanup INT TERM EXIT

"${bin}/skep-agent" -dev &
agent_pid=$!
"${bin}/skep-api" -dev "$@" &
api_pid=$!

# Stop everything as soon as either process exits.
while kill -0 "${agent_pid}" 2>/dev/null && kill -0 "${api_pid}" 2>/dev/null; do
	sleep 1
done
