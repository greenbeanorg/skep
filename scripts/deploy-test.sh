#!/bin/sh
# Copies FreeBSD builds to a test host and installs them root-owned,
# verified, and on disk. A development tool for the lab test host; it is
# not how Skep is installed.
#
#   scripts/deploy-test.sh HOST
#   scripts/deploy-test.sh HOST DIR FILE...
#
# With HOST alone, the FreeBSD builds go where the package puts them:
# bin/freebsd-amd64/skep-agent and skep-api to /usr/local/sbin, skepctl to
# /usr/local/bin (the rc.d scripts' defaults). With DIR, the FILEs go to
# DIR: test binaries, or agent.json to /usr/local/etc/skep.
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
# With HOST alone, the script then restarts skep_api and skep_agent (stop
# the API, stop the agent, start the agent, start the API) and succeeds
# only if each running service is the deployed build: the inode of the
# process's text (fstat) is the inode of the installed file, and that
# file's -version names the commit of the local checkout (git rev-parse
# --short HEAD; a checkout with uncommitted changes is refused, because its
# build would claim a commit it is not). A replaced binary does not change a running process, so
# an install without the restart leaves the old build serving.
#
# Needs passwordless sudo on HOST.
set -eu

if [ $# -eq 1 ]; then
	host="$1"
	commit="$(git rev-parse --short HEAD)"
	if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
		echo "deploy-test: refusing: the checkout has uncommitted changes, so the build is not ${commit}; commit first" >&2
		exit 2
	fi
	# The package layout, as two runs of this script.
	"$0" "${host}" /usr/local/sbin bin/freebsd-amd64/skep-agent bin/freebsd-amd64/skep-api
	"$0" "${host}" /usr/local/bin bin/freebsd-amd64/skepctl
	echo "deploy-test: restarting skep_api and skep_agent on ${host}"
	ssh "${host}" "set -e
		sudo service skep_api stop
		sudo service skep_agent stop
		sudo service skep_agent start
		sudo service skep_api start"
	# Each service: wait for its pid file, let it run a few seconds, then
	# compare the running text with the installed file and its commit.
	ssh "${host}" "set -e
		verify() {
			svc=\$1; bin=\$2; pidfile=\$3
			i=0
			while ! sudo test -s \"\$pidfile\"; do
				i=\$((i + 1))
				if [ \$i -gt 20 ]; then echo \"deploy-test: \$svc wrote no pid file\" >&2; exit 1; fi
				sleep 0.5
			done
			sleep 3
			pid=\$(sudo cat \"\$pidfile\")
			if ! sudo kill -0 \"\$pid\" 2>/dev/null; then
				echo \"deploy-test: \$svc (pid \$pid) is not running\" >&2; exit 1
			fi
			running=\$(sudo fstat -p \"\$pid\" | awk '\$4 == \"text\" { print \$6 }')
			installed=\$(stat -f %i \"\$bin\")
			if [ \"\$running\" != \"\$installed\" ]; then
				echo \"deploy-test: \$svc (pid \$pid) runs inode \$running, not the installed \$bin (inode \$installed)\" >&2; exit 1
			fi
			ver=\$(\"\$bin\" -version)
			case \"\$ver\" in
			*\"(commit ${commit},\"*) ;;
			*) echo \"deploy-test: \$svc runs \$ver, not commit ${commit}\" >&2; exit 1 ;;
			esac
			echo \"deploy-test: \$svc pid \$pid runs \$bin: \$ver\"
		}
		verify skep_agent /usr/local/sbin/skep-agent /var/run/skep/agent.pid
		verify skep_api /usr/local/sbin/skep-api /var/run/skep/api.pid"
	echo "deploy-test: ${host} runs ${commit}"
	exit 0
fi
if [ $# -lt 3 ]; then
	echo "usage: $0 HOST | $0 HOST DIR FILE..." >&2
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
