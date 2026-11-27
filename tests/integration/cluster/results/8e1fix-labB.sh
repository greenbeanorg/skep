#!/bin/bash
# 8e-1 fix lab (8e1fix: a copy of 8e1-labB.sh, the work directory skep-8e1fix).
# 8e-1 lab (from 8d2-labB.sh; 8e1 adds p8445 HOST, nodekey HOST,
# apikeys HOST, probe HOST ARGS, and copies forwardprobe in deploy): smith (S), langrishe (L), star (T) and blazanov (B) from merrick over
# ssh as aba (sudo on the VMs). The package stage (make stage, dist/pkg)
# is copied to ~/skep-8c2 on each VM (deploy); "upgrade HOST" runs
# skep-setup upgrade alone (no hand restart of skep_cluster). Commands that
# confirm take -cluster-id or --yes (no terminal over ssh). API calls go
# through 8d2-api.py (pinned TLS; passwords never printed). 8d2 adds blazanov
# to deploy, vid, rootcheck and pids, and: drain HOST (the agent's drain,
# read as root), agentaudit HOST PATTERN (count of the agent's audit
# entries), jls HOST.
# Usage: 8c2-labB.sh deploy | upgrade HOST | wait3 | roles | st HOST | pids | vid |
#        rootcheck | audit HOST | auditgrep HOST PATTERN | sc HOST ARGS... |
#        svc HOST SERVICE stop|start | importstate | clusterid | sock HOST root|skep ARGS...
set -u
S=10.79.20.81; L=10.79.20.82; T=10.79.20.83; B=10.79.20.84
SC=/usr/local/sbin/skep-cluster
W=skep-8e1fix
on() { local h=$1; shift; ssh -o BatchMode=yes -o ConnectTimeout=10 aba@$h "$@"; }
h() { echo; echo "=== $(date -u +%H:%M:%SZ) $* ==="; }
name() { case $1 in $S) echo smith;; $L) echo langrishe;; $T) echo star;; $B) echo blazanov;; *) echo "$1";; esac; }
st() { on $1 "sudo $SC status"; }
healthy3() { for x in $S $L $T; do local s; s=$(st $x 2>/dev/null) || return 1
	[ "$(echo "$s" | grep -c 'member, voter')" = 3 ] && echo "$s" | grep -q '^Health: *ok' || return 1; done; }
wait3() { local i=0; while [ $i -lt 300 ]; do healthy3 && { echo "three voters, health ok on all after ${i}s"; return 0; }; sleep 3; i=$((i+3)); done; echo "TIMEOUT: not healthy"; return 1; }
pids() { for x in $S $L $T $B; do echo "$(name $x): $(on $x 'for s in skep-agent skep-cluster skep-api; do printf "%s=%s " $s "$(pgrep -x $s | tr "\n" " ")"; done')"; done; }

case ${1:-} in
deploy)
	for x in $S $L $T $B; do
		h "deploy to $(name $x)"
		on $x "rm -rf $W && mkdir -p $W"
		tar -C dist -cf - pkg | on $x "tar -C $W -xf -"
		scp -q bin/freebsd-amd64-lab/agentsock aba@$x:$W/agentsock && on $x "chmod 755 $W/agentsock"
		scp -q bin/freebsd-amd64-lab/forwardprobe aba@$x:$W/forwardprobe && on $x "chmod 755 $W/forwardprobe"
		on $x "sha256 -q $W/pkg/stage/usr/local/sbin/skep-cluster"
	done ;;
upgrade)
	x=$2
	h "upgrade $(name $x) with skep-setup upgrade alone"
	echo "before: $(on $x 'printf "cluster pid %s, " "$(pgrep -x skep-cluster)"; sudo skep-api admin list --import-state')"
	on $x "cd $W/pkg && rm -rf out && mkdir out && pkg create -M +MANIFEST -p plist -r stage -o out && sudo pkg install -y -f ./out/skep-0.1.0.pkg >/dev/null; echo pkg-exit=\$?"
	on $x "sudo skep-setup upgrade -yes -keep 50; echo upgrade-exit=\$?"
	echo "after: $(on $x 'printf "cluster pid %s, " "$(pgrep -x skep-cluster)"; /usr/local/sbin/skep-cluster version; sudo skep-api admin list --import-state')" ;;
wait3) wait3 ;;
waitv)	# waitv N HOST...: N voters in state member and health ok on each HOST
	n=$2; shift 2; i=0
	while [ $i -lt 300 ]; do ok=1
		for x in "$@"; do s=$(st $x 2>/dev/null) || { ok=0; break; }
			[ "$(echo "$s" | grep -c 'member, voter')" = "$n" ] && echo "$s" | grep -q '^Health: *ok' || { ok=0; break; }; done
		[ $ok = 1 ] && { echo "$n voters, health ok on $* after ${i}s"; exit 0; }
		sleep 3; i=$((i+3)); done
	echo "TIMEOUT: not $n voters with health ok"; exit 1 ;;
roles)
	for x in $S $L $T; do echo "$(name $x) $x $(st $x | sed -n 's/^Raft: *\([a-z]*\).*/\1/p')"; done ;;
st) st "$2" ;;
pids) pids ;;
vid) for x in $S $L $T $B; do echo "$(name $x): vid=$(on $x 'sysctl -n hw.vmm.vmx.cap.virtual_interrupt_delivery')"; done ;;
rootcheck) for x in $S $L $T $B; do echo "$(name $x): root-owned [$(on $x 'sudo find /var/db/skep/cluster -user root | tr "\n" " "')]"; done ;;
audit) on $2 "sudo $SC audit verify; echo exit=\$?" ;;
auditgrep) on $2 "sudo sh -c 'cat /var/db/skep/cluster/audit/audit-*.jsonl' | grep -c -- '$3'; true" ;;
sc) x=$2; shift 2; h "$(name $x): skep-cluster $*"; on $x "sudo $SC $*"; echo "exit=$?" ;;
svc) h "$(name $2): service $3 $4"; on $2 "sudo service $3 $4"; echo "exit=$?" ;;
importstate) for x in $S $L $T; do echo "$(name $x): $(on $x 'sudo skep-api admin list --import-state')"; done ;;
clusterid) st $S | sed -n 's/^Cluster ID: *//p' ;;
sock) x=$2; who=$3; shift 3
	if [ "$who" = root ]; then on $x "sudo ./$W/agentsock $*"; else on $x "sudo -u skep ./$W/agentsock $*"; fi ;;
drain) on $2 "sudo ./$W/agentsock -actor lab-8e1 GET /drain" ;;
agentaudit) on $2 "sudo skep-agent audit export --unsealed 2>/dev/null" | grep -c -- "$3"; true ;;
jls) on $2 "sudo jls name | sort | tr '\n' ' '"; echo ;;
p8445) on $2 "sudo sockstat -46l -p 8445 | tail -n +2"; echo "(end of 8445 sockets on $(name $2))" ;;
nodekey) on $2 "sudo ls -la /var/db/skep/api/nodekey 2>&1" ;;
apikeys) on $2 "sudo sh -c 'cat /var/db/skep/cluster/audit/audit-*.jsonl' | grep -c 'member.api_key'; true" ;;
probe) x=$2; shift 2; on $x "install -m 755 $W/forwardprobe /tmp/forwardprobe-8e1fix && cd /tmp && sudo -u skep /tmp/forwardprobe-8e1fix $*; rm -f /tmp/forwardprobe-8e1fix" ;;
*) sed -n '2,16p' "$0"; exit 2 ;;
esac
