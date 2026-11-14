#!/bin/sh
# Copies FreeBSD builds to a test host and installs them, verified and on
# disk: each file goes to a temporary name in the target directory, its
# size and SHA-256 are compared with the local build, it is renamed into
# place, and sync(8) runs, so a hard reset of the host right afterwards
# cannot leave a truncated or empty binary behind. A development tool for
# the lab test host; it is not how Skep is installed.
#
#   scripts/deploy-test.sh HOST DIR [FILE...]
#
# FILE defaults to bin/freebsd-amd64/skep-agent, skep-api, and skepctl. DIR
# is created if missing and must be writable by the SSH user.
set -eu

if [ $# -lt 2 ]; then
	echo "usage: $0 HOST DIR [FILE...]" >&2
	exit 2
fi
host="$1"
dir="$2"
shift 2
if [ $# -eq 0 ]; then
	set -- bin/freebsd-amd64/skep-agent bin/freebsd-amd64/skep-api bin/freebsd-amd64/skepctl
fi

ssh "${host}" "mkdir -p '${dir}'"
for f in "$@"; do
	name="$(basename "$f")"
	size="$(wc -c <"$f" | tr -d ' ')"
	sum="$(sha256sum "$f" | cut -d' ' -f1)"
	scp -q "$f" "${host}:${dir}/.${name}.new"
	# Verified before it replaces anything; renamed only when it matches.
	ssh "${host}" "set -e
		f='${dir}/.${name}.new'
		got_size=\$(stat -f %z \"\$f\"); got_sum=\$(sha256 -q \"\$f\")
		if [ \"\$got_size\" != '${size}' ] || [ \"\$got_sum\" != '${sum}' ]; then
			echo 'deploy-test: ${name} does not match the local build' >&2; rm -f \"\$f\"; exit 1
		fi
		chmod 0755 \"\$f\"
		mv -f \"\$f\" '${dir}/${name}'"
	echo "deploy-test: ${host}:${dir}/${name} ${size} bytes sha256 ${sum}"
done
ssh "${host}" sync
echo "deploy-test: synced"
