#!/bin/sh
# Runs the browser tests with skep-api trusted by its public key pin (ADR
# 0010 amendment x, option c), never by ignoring certificate errors:
# Chromium gets the pin (--ignore-certificate-errors-spki-list, set in
# web/playwright.config.ts from SKEP_E2E_PIN), and Playwright's Node-side
# request client trusts exactly the certificate that carries the pinned key
# (NODE_EXTRA_CA_CERTS; Node reads it only at start, so it is set here and
# not in the configuration).
#
# Local mode (default): makes skep-api's key first (skep-api tls
# regenerate) in a new state directory, takes the pin from skep-api tls
# fingerprint, checks the certificate file against it, and hands the
# directory to scripts/e2e-stack.sh, which Playwright starts.
#
# External mode (SKEP_E2E_EXTERNAL=1): a skep-api already runs at
# SKEP_E2E_LISTEN (host:port) with the agent's test build behind it;
# SKEP_E2E_PIN must be its pin (sha256:<hex>, from skep-api tls fingerprint
# on that host, compared out of band). Its certificate is fetched and used
# only if it carries that key.
#
#   SKEP_BIN_DIR   where the binaries are (default bin)
# Extra arguments go to playwright test.
set -eu

cd "$(dirname "$0")/.."
bin="${SKEP_BIN_DIR:-bin}"
dir="$(mktemp -d /tmp/skep-e2e.XXXXXX)"
trap 'rm -rf "${dir}"' INT TERM EXIT

if [ "${SKEP_E2E_EXTERNAL:-}" = "1" ]; then
	: "${SKEP_E2E_LISTEN:?SKEP_E2E_LISTEN (host:port of the running skep-api) is required in external mode}"
	: "${SKEP_E2E_PIN:?SKEP_E2E_PIN (skep-api tls fingerprint on that host) is required in external mode}"
	cert="${dir}/skep-api-cert.pem"
	node scripts/e2e-trust.mjs fetch "${SKEP_E2E_LISTEN}" "${SKEP_E2E_PIN}" "${cert}"
	pin="${SKEP_E2E_PIN}"
else
	if [ ! -x "${bin}/skep-api" ]; then
		echo "web-e2e: ${bin}/skep-api is missing; run 'make build' first" >&2
		exit 1
	fi
	"${bin}/skep-api" tls regenerate -dev -data-dir "${dir}/api" >/dev/null
	pin="$("${bin}/skep-api" tls fingerprint -dev -data-dir "${dir}/api" | awk '$1 == "public" && $2 == "key:" { print $3 }')"
	cert="$(find "${dir}/api" -name cert.pem -path '*/tls/*')"
	node scripts/e2e-trust.mjs check "${cert}" "${pin}"
	export SKEP_E2E_DIR="${dir}"
fi

echo "web-e2e: trusting skep-api's key ${pin}"
cd web
SKEP_E2E_PIN="${pin}" NODE_EXTRA_CA_CERTS="${cert}" npx playwright test "$@"
