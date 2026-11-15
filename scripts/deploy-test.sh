#!/bin/sh
# Copies FreeBSD builds to a test host and installs them root-owned,
# verified, and on disk. A development tool for the lab test host; it is
# not how Skep is installed.
#
#   scripts/deploy-test.sh HOST DIR [FILE...]
#
# FILE defaults to bin/freebsd-amd64/skep-agent, skep-api, and skepctl.
# Each file is copied to a private staging directory on HOST (mktemp, the
# SSH user's), its size and SHA-256 are compared with the local build, and
# only then is it installed with sudo into DIR under a temporary name,
# checked again, and renamed into place; sync(8) runs at the end, so a hard
# reset right afterwards cannot leave a truncated binary behind.
#
# Ownership: DIR is created or made root:wheel 0755. Programs are installed
# root:wheel 0555. A file named agent.json is installed root:wheel 0600:
# only the root agent reads it (skep-api never does). Any other file is
# refused. The agent runs as root, so nothing it executes or reads as
# configuration may be writable by another user; the script warns when a
# directory above DIR is not root's, because whoever owns it can replace
# DIR itself.
#
# Needs passwordless sudo on HOST.
set -eu

if [ $# -lt 2 ]; then
	echo "usage: $0 HOST DIR [FILE...]" >&2
	exit 2
fi
host="$1"
dir="$2"
shift 2
case "${dir}" in
/*) ;;
*)
	echo "deploy-test: DIR must be an absolute path" >&2
	exit 2
	;;
esac
if [ $# -eq 0 ]; then
	set -- bin/freebsd-amd64/skep-agent bin/freebsd-amd64/skep-api bin/freebsd-amd64/skepctl
fi

# The mode of each file, decided before anything is copied.
for f in "$@"; do
	case "$(basename "$f")" in
	agent.json) ;;
	*.json | *.conf | *.sample)
		echo "deploy-test: refusing $f: only programs and agent.json are installed" >&2
		exit 2
		;;
	*)
		if [ ! -x "$f" ]; then
			echo "deploy-test: refusing $f: not an executable" >&2
			exit 2
		fi
		;;
	esac
done

stage="$(ssh "${host}" 'mktemp -d /tmp/skep-deploy.XXXXXX')"
cleanup() { ssh "${host}" "rm -rf '${stage}'" || true; }
trap cleanup EXIT INT TERM

ssh "${host}" "set -e
	sudo install -d -o root -g wheel -m 0755 '${dir}'
	sudo chown root:wheel '${dir}'
	sudo chmod 0755 '${dir}'
	p='${dir}'
	while [ \"\$p\" != / ]; do
		p=\$(dirname \"\$p\")
		owner=\$(stat -f %Su \"\$p\")
		if [ \"\$owner\" != root ]; then
			echo \"deploy-test: warning: \$p is owned by \$owner, who can replace ${dir}\" >&2
		fi
	done"

for f in "$@"; do
	name="$(basename "$f")"
	mode=0555
	[ "${name}" = agent.json ] && mode=0600
	size="$(wc -c <"$f" | tr -d ' ')"
	sum="$(sha256sum "$f" | cut -d' ' -f1)"
	scp -q "$f" "${host}:${stage}/${name}"
	ssh "${host}" "set -e
		check() {
			got_size=\$(sudo stat -f %z \"\$1\"); got_sum=\$(sudo sha256 -q \"\$1\")
			if [ \"\$got_size\" != '${size}' ] || [ \"\$got_sum\" != '${sum}' ]; then
				echo \"deploy-test: \$1 does not match the local build of ${name}\" >&2; sudo rm -f \"\$1\"; exit 1
			fi
		}
		check '${stage}/${name}'
		sudo install -o root -g wheel -m ${mode} '${stage}/${name}' '${dir}/.${name}.new'
		check '${dir}/.${name}.new'
		sudo mv -f '${dir}/.${name}.new' '${dir}/${name}'"
	echo "deploy-test: ${host}:${dir}/${name} root:wheel ${mode} ${size} bytes sha256 ${sum}"
done
ssh "${host}" sync
echo "deploy-test: synced"
