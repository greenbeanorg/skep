#!/bin/bash
# 8b-1 fix round, job 3: upgrade smith, langrishe and star one at a time to
# the branch head's package, then the four lab checks. Driven from merrick
# over ssh as aba (sudo on the lab VMs). The package stage (make stage,
# dist/pkg) and the FreeBSD probe are copied to ~/skep-8b1fix on each VM
# first, as in 8b1-lab-upgrade.sh.
# Usage: 8b1-fix-lab.sh upgrade HOST | wait | check1 | check2 | check3 | check4 | pids
set -u
S=10.79.20.81; L=10.79.20.82; T=10.79.20.83
P=${PROBE:-bin/cluster-probe}   # linux probe on merrick
SC=/usr/local/sbin/skep-cluster
on() { local h=$1; shift; ssh -o BatchMode=yes -o ConnectTimeout=10 aba@$h "$@"; }
h() { echo; echo "=== $(date -u +%H:%M:%SZ) $* ==="; }
name() { case $1 in $S) echo smith;; $L) echo langrishe;; $T) echo star;; esac; }
pids() { for x in $S $L $T; do echo "$(name $x): $(on $x 'for s in skep-agent skep-api skep-cluster; do printf "%s=%s " $s "$(pgrep -x $s | tr "\n" " ")"; done; echo "vid=$(sysctl -n hw.vmm.vmx.cap.virtual_interrupt_delivery)"')"; done; }
status() { on $1 "sudo $SC status"; }
field() { status $1 | sed -n "s/^$2: *//p" | head -1; }
pin() { field $1 'Node key pin'; }
# healthy: every host shows three "member, voter" lines and health ok.
healthy() { for x in $S $L $T; do local st; st=$(status $x) || return 1
	[ "$(echo "$st" | grep -c 'member, voter')" = 3 ] && echo "$st" | grep -q '^Health: *ok' || return 1; done; }
waithealthy() { local i=0; while [ $i -lt ${1:-120} ]; do healthy && { echo "healthy after ${i}s"; return 0; }; sleep 2; i=$((i+2)); done; echo "TIMEOUT: not healthy"; return 1; }

upgrade() { # upgrade HOST: package, skep-setup upgrade, restart skep_cluster by hand
	local x=$1 ck="-mgmt vtnet0"
	[ "$x" = "$S" ] || ck="-mgmt vtnet0 -pool zroot -no-racct"
	h "upgrade $(name $x)"
	on $x "sh skep-8b1fix/8b1-lab-upgrade.sh skep-8b1fix $ck"
	h "restart skep_cluster on $(name $x) by hand"
	on $x 'sudo service skep_cluster restart; echo "exit=$?"'
	waithealthy 120
	pids
}

check1() {
	h "1. status and health on all three"
	for x in $S $L $T; do echo "--- $(name $x)"; status $x; on $x "sudo $SC health; echo health-exit=\$?"; done
}

check2() {
	for x in $S $L $T; do
		h "2. $(name $x): service skep_cluster stop (must return within 30 s)"
		on $x 's=$(date +%s); sudo service skep_cluster stop; e=$?; echo "exit=$e seconds=$(( $(date +%s) - s ))"'
		on $x 'sudo service skep_cluster start; echo "exit=$?"'
		waithealthy 120
	done
	pids
}

check3() {
	h "3. a pending key: admitted, Raft connection closed before any RPC is read, withdrawn"
	local K=/tmp/8b1fix-probekey
	rm -rf $K $K.txt
	$P keygen -dir $K -name probe -address 10.79.20.80:8444 | tee $K.txt
	local PREQ PID PPIN
	PREQ=$(grep '^skep-join-request:' $K.txt); PID=$(sed -n 's/^node ID //p' $K.txt); PPIN=$(sed -n 's/^pin //p' $K.txt)
	on $S "sudo $SC admit '$PREQ'" | head -3
	sleep 3
	for x in $S $L $T; do echo "$(name $x) pending: $(status $x | grep -c "probe .*$PPIN")"; done
	for x in $S $L $T; do
		echo "--- $(name $x)"
		$P rpc -addr $x:8444 -server-pin $(pin $x) -key-dir $K -claim-id $PID -claim-addr 10.79.20.80:8444 -op raft
		sleep 1
		echo "receiver's log for the pending key's pin:"
		on $x "sudo grep -h '$PPIN' /var/log/daemon.log | tail -3"
	done
	on $S "sudo $SC admit --withdraw probe"; echo "exit=$?"
	sleep 3
	for x in $S $L $T; do echo "$(name $x): pending section lines: $(status $x | grep -c 'Pending admissions')"; done
	rm -rf $K $K.txt
}

check4() {
	h "4. admit of a request whose address has port 9444"
	local REQ
	REQ=$($P request -name p9444 -address 10.79.20.99:9444 -pin sha256:$(printf '0%.0s' $(seq 64)))
	on $S "sudo $SC admit '$REQ'"; echo "exit=$?"
	for x in $S $L $T; do echo "$(name $x): pending section lines: $(status $x | grep -c 'Pending admissions')"; done
}

case ${1:-} in
upgrade) upgrade "$2" ;;
wait) waithealthy 120 ;;
check1|check2|check3|check4|pids) "$1" ;;
*) echo "usage: $0 upgrade HOST | wait | check1..check4 | pids" >&2; exit 2 ;;
esac
