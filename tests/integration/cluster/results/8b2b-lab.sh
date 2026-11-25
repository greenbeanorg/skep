#!/bin/bash
# 8b-2b lab (job 9): drive smith (S), langrishe (L) and star (T) from merrick
# over ssh as aba (sudo on the VMs). The package stage (make stage,
# dist/pkg), the FreeBSD cluster-probe and 8b2b-lab-upgrade.sh are copied to
# ~/skep-8b2b on each VM first (deploy). Commands that confirm take
# -cluster-id (no terminal over ssh).
# Usage: 8b2b-lab.sh deploy | upgrade HOST | wait3 | wait2 A B | st HOST | hl HOST |
#        pids | term HOST | leader | rootcheck | audit HOST | count HOST | sc HOST ARGS...
set -u
S=10.79.20.81; L=10.79.20.82; T=10.79.20.83
SC=/usr/local/sbin/skep-cluster
W=skep-8b2b
on() { local h=$1; shift; ssh -o BatchMode=yes -o ConnectTimeout=10 aba@$h "$@"; }
h() { echo; echo "=== $(date -u +%H:%M:%SZ) $* ==="; }
name() { case $1 in $S) echo smith;; $L) echo langrishe;; $T) echo star;; *) echo "$1";; esac; }
pids() { for x in $S $L $T; do echo "$(name $x): $(on $x 'for s in skep-agent skep-api skep-cluster; do printf "%s=%s " $s "$(pgrep -x $s | tr "\n" " ")"; done; echo "vid=$(sysctl -n hw.vmm.vmx.cap.virtual_interrupt_delivery) vmm=$(ls /dev/vmm 2>/dev/null | wc -l | tr -d " ")"')"; done; }
st() { on $1 "sudo $SC status"; }
hl() { on $1 "sudo $SC health; echo health-exit=\$?"; }
healthyN() { local n=$1; shift; for x in "$@"; do local s; s=$(st $x) || return 1
	[ "$(echo "$s" | grep -c 'member, voter')" = "$n" ] && echo "$s" | grep -q '^Health: *ok' || return 1; done; }
waitN() { local n=$1; shift; local i=0; while [ $i -lt 240 ]; do healthyN $n "$@" && { echo "healthy ($n voters on $*) after ${i}s"; return 0; }; sleep 2; i=$((i+2)); done; echo "TIMEOUT: not healthy"; return 1; }
leader() { for x in $S $L $T; do if st $x 2>/dev/null | grep -q '^Raft: *leader'; then echo $x; return; fi; done; }

case ${1:-} in
deploy)
	for x in $S $L $T; do
		h "deploy to $(name $x)"
		on $x "rm -rf $W && mkdir -p $W"
		tar -C dist -cf - pkg | on $x "tar -C $W -xf -"
		scp -q bin/freebsd-amd64-test/cluster-probe tests/integration/cluster/results/8b2b-lab-upgrade.sh aba@$x:$W/
		on $x "ls $W; sha256 -q $W/pkg/stage/usr/local/sbin/skep-cluster"
	done ;;
upgrade)
	x=$2; ck="-mgmt vtnet0"; [ "$x" = "$S" ] || ck="-mgmt vtnet0 -pool zroot -no-racct"
	h "upgrade $(name $x)"
	on $x "sh $W/8b2b-lab-upgrade.sh $W $ck"
	pids ;;
wait3) waitN 3 $S $L $T ;;
wait2) waitN 2 "$2" "$3" ;;
st) st "$2" ;;
hl) hl "$2" ;;
pids) pids ;;
term) st "$2" | sed -n 's/^Raft: *//p' ;;
leader) leader ;;
rootcheck) for x in $S $L $T; do echo "$(name $x): root-owned [$(on $x 'sudo find /var/db/skep/cluster -user root | tr "\n" " "')]"; done ;;
audit) on $2 "sudo $SC audit verify; echo exit=\$?" ;;
count) on $2 "sudo cat /var/db/skep/cluster/audit/audit-*.jsonl | wc -l | tr -d ' '; sudo cat /var/db/skep/cluster/audit/audit-*.jsonl | grep -o '\"kind\":\"entry\",\"epoch\":[0-9]*,\"index\":[0-9]*' | sort | uniq -d | wc -l | tr -d ' '" ;;
sc) x=$2; shift 2; h "$(name $x): skep-cluster $*"; on $x "sudo $SC $*"; echo "exit=$?" ;;
*) echo "usage: $0 deploy | upgrade HOST | wait3 | wait2 A B | st HOST | hl HOST | pids | term HOST | leader | rootcheck | audit HOST | count HOST | sc HOST ARGS..." >&2; exit 2 ;;
esac
