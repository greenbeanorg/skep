#!/bin/sh
# Runs the stack the browser tests drive (ADR 0008, decision 8): a real
# skep-api built with the web interface embedded, in front of the agent's
# test build, which submits one cancellable test.sleep task at start so that
# there is something to cancel. Everything lives in a temporary directory
# that is removed when the script exits. An administrator is created with
# the local command, as an operator would.
#
# Playwright starts this script (web/playwright.config.ts) and waits for
# the health endpoint; scripts/web-e2e.sh starts Playwright. It is a development tool on a development machine
# and proves nothing about FreeBSD.
#
#   SKEP_E2E_LISTEN    skep-api's listen address (default 127.0.0.1:8443)
#   SKEP_E2E_PASSWORD  the administrator's password (default below)
#   SKEP_BIN_DIR       where the binaries are (default bin)
#   SKEP_E2E_DIR       the state directory to use (default: a new one)
set -eu

cd "$(dirname "$0")/.."
bin="${SKEP_BIN_DIR:-bin}"
listen="${SKEP_E2E_LISTEN:-127.0.0.1:8443}"
password="${SKEP_E2E_PASSWORD:-correct horse battery staple}"

if [ ! -x "${bin}/skep-api" ]; then
	echo "e2e-stack: ${bin}/skep-api is missing; run 'make build' first" >&2
	exit 1
fi
if ! grep -a -q 'src="/theme.js"' "${bin}/skep-api"; then
	echo "e2e-stack: ${bin}/skep-api was built without the web interface; run 'make build'" >&2
	exit 1
fi
# The agent's test build carries the test.sleep task type. It is never
# installed anywhere. skep-api refuses an agent of another build (503
# agent_version_mismatch), so it is stamped with skep-api's version and
# commit, and rebuilt when it was stamped otherwise.
api_build=$("${bin}/skep-api" -version | sed -n 's/^skep-api \([^ ]*\) (commit \([^,]*\),.*/\1 \2/p')
api_version=${api_build% *}
api_commit=${api_build#* }
if [ -z "${api_build}" ]; then
	echo "e2e-stack: cannot read the version of ${bin}/skep-api" >&2
	exit 1
fi
if [ ! -x "${bin}/skep-agent-testbuild" ] ||
	! "${bin}/skep-agent-testbuild" -version | grep -q "^skep-agent ${api_version} (commit ${api_commit},"; then
	echo "e2e-stack: building ${bin}/skep-agent-testbuild (${api_version}, commit ${api_commit})"
	CGO_ENABLED=0 go build -trimpath -tags skep_testtasks \
		-ldflags "-X github.com/greenbeanorg/skep/pkg/version.Version=${api_version} -X github.com/greenbeanorg/skep/pkg/version.Commit=${api_commit}" \
		-o "${bin}/skep-agent-testbuild" ./cmd/skep-agent
fi

# A short directory: the socket path is limited to about 104 bytes.
# scripts/web-e2e.sh passes its own (SKEP_E2E_DIR), with skep-api's key
# already made so that the tests can pin it; that directory is its to
# remove.
if [ -n "${SKEP_E2E_DIR:-}" ]; then
	dir="${SKEP_E2E_DIR}"
	own_dir=""
else
	dir="$(mktemp -d /tmp/skep-e2e.XXXXXX)"
	own_dir=1
fi
agent_pid=""
api_pid=""
cleanup() {
	trap - INT TERM EXIT
	[ -n "${api_pid}" ] && kill "${api_pid}" 2>/dev/null || true
	[ -n "${agent_pid}" ] && kill "${agent_pid}" 2>/dev/null || true
	wait 2>/dev/null || true
	if [ -n "${own_dir}" ]; then rm -rf "${dir}"; fi
}
trap cleanup INT TERM EXIT

socket="${dir}/agent/run/agent.sock"
# The agent's confinement for the VM tests: on a development machine the
# host is simulated, so the pool and the bridge exist only in memory. The
# media directory is real and holds a stand-in ISO.
mkdir -p "${dir}/media"
printf 'not really an ISO\n' >"${dir}/media/test-install.iso"
cat >"${dir}/agent.json" <<EOF
{
  "storage": { "pools": ["devpool"], "dataset_root": "skep", "media_dir": "${dir}/media" },
  "network": { "bridges": ["bridge0"], "management_interfaces": ["vtnet0"] }
}
EOF
SKEP_TEST_SLEEP=8m "${bin}/skep-agent-testbuild" -dev -config "${dir}/agent.json" -data-dir "${dir}/agent" -log-level warn &
agent_pid=$!

printf '%s\n' "${password}" | "${bin}/skep-api" admin create --username andy --display-name "Andy Test" \
	--password-stdin -dev -data-dir "${dir}/api" >/dev/null

"${bin}/skep-api" -dev -data-dir "${dir}/api" -agent-socket "${socket}" -listen "${listen}" -log-level warn &
api_pid=$!
echo "e2e-stack: skep-api on https://${listen}, state in ${dir}"

while kill -0 "${agent_pid}" 2>/dev/null && kill -0 "${api_pid}" 2>/dev/null; do
	sleep 1
done
