#!/bin/bash
# 8b-1 lab tests (job 5), driven from merrick over ssh as aba (sudo on the
# lab VMs). Each case uses the commands as skep-cluster prints them. The
# probe runs from merrick (bin/cluster-probe, linux) and, where it needs a
# member's key, on the member as the skep-cluster account (read only).
# Usage: 8b1-lab-run.sh CASE...   (cases 1 to 8, or "pids")
set -u
S=10.79.20.81; L=10.79.20.82; T=10.79.20.83
P=${PROBE:-bin/cluster-probe}          # linux probe on merrick
RP=${REMOTE_PROBE:-skep-8b1/cluster-probe}  # freebsd probe on the VMs (in aba's home)
SC=/usr/local/sbin/skep-cluster
on() { local h=$1; shift; ssh -o BatchMode=yes -o ConnectTimeout=10 aba@$h "$@"; }
h() { echo; echo "=== $(date -u +%H:%M:%SZ) $* ==="; }
name() { case $1 in $S) echo smith;; $L) echo langrishe;; $T) echo star;; esac; }
pids() { for x in $S $L $T; do echo "$(name $x): $(on $x 'for s in skep-agent skep-api skep-cluster; do printf "%s=%s " $s "$(pgrep -x $s | tr "\n" " ")"; done')"; done; }
status() { on $1 "sudo $SC status"; }
field() { status $1 | sed -n "s/^$2: *//p" | head -1; }
pin() { field $1 'Node key pin'; }
nodeid() { field $1 'Node ID'; }
cid() { field $1 'Cluster ID'; }
waitfor() { # waitfor HOST SECONDS GREP-PATTERN
	local i=0; while [ $i -lt $2 ]; do status $1 | grep -q -- "$3" && return 0; sleep 2; i=$((i+2)); done; echo "TIMEOUT waiting for '$3' on $(name $1)"; return 1; }
rootfiles() { for x in "$@"; do echo "$(name $x) root-owned files in the data directory: $(on $x 'sudo find /var/db/skep/cluster -user root | wc -l')"; done; }
memfile() { on $1 "sudo stat -f '%Sp %Su:%Sg %N' /var/db/skep/cluster/membership.json; sudo grep -c -i pending /var/db/skep/cluster/membership.json; sudo cat /var/db/skep/cluster/membership.json"; }

case1() {
	h "1. star after init: nothing listens on 8444; admit on star refused (no listen address)"
	on $T 'sockstat -4l -p 8444; sockstat -6l -p 8444'
	REQ=$($P request -name nobody -address 10.79.20.99:8444 -pin sha256:$(printf '0%.0s' $(seq 64)))
	on $T "sudo $SC admit '$REQ'"; echo "exit=$?"
}

case2() {
	h "2. langrishe: join-request"
	on $L "sudo $SC join-request -address 10.79.20.82" | tee /tmp/8b1-req-l.txt; echo "exit=${PIPESTATUS[0]}"
	REQ=$(grep '^skep-join-request:' /tmp/8b1-req-l.txt | head -1)
	echo "pin in the request: $(grep 'Node key pin' /tmp/8b1-req-l.txt)"; echo "status on langrishe: Node key pin: $(pin $L)"
	h "2. smith: admit"
	on $S "sudo $SC admit '$REQ'" | tee /tmp/8b1-adm-l.txt; echo "exit=${PIPESTATUS[0]}"
	ADM=$(grep '^skep-admission:' /tmp/8b1-adm-l.txt | head -1)
	C=$(cid $S)
	h "2. langrishe: join -cluster-id $C"
	on $L "sudo $SC join -cluster-id $C '$ADM'"; echo "exit=$?"
	waitfor $L 90 'langrishe[^ ]* .*member, voter'
	waitfor $S 60 'langrishe[^ ]* .*member, voter'
	for x in $S $L; do h "2. state on $(name $x)"; status $x; memfile $x; done
	rootfiles $S $L
}

case3() {
	C=$(cid $S)
	h "3. smith: join-request refused (two servers)"
	on $S "sudo $SC join-request -address 10.79.20.81"; echo "exit=$?"
	h "3. smith: join refused (two servers), with an admission naming smith"
	ADM=$($P admission -cluster-id $C -for "$(nodeid $S),$(field $S 'Node name'),10.79.20.81:8444,$(pin $S)" -member "$(nodeid $L),$(field $L 'Node name'),10.79.20.82:8444,$(pin $L)")
	on $S "sudo $SC join -cluster-id $C '$ADM'"; echo "exit=$?"
	waitfor $S 30 'Health: *ok'
	h "3. langrishe stopped: admit on smith does not commit"
	on $L 'sudo service skep_cluster stop'; sleep 6
	on $S "sudo $SC health"; echo "health-exit=$?"
	REQ=$($P request -name nocommit -address 10.79.20.98:8444 -pin sha256:$(printf '1%.0s' $(seq 64)))
	on $S "sudo $SC admit '$REQ'"; echo "exit=$?"
	on $L 'sudo service skep_cluster start'; waitfor $S 60 'Health: *ok'
	h "3. pending entries after langrishe is back (nocommit must not be there)"
	status $S | sed -n '/^Pending/,$p'
}

case3join() {
	C=$(cid $S)
	h "3. smith: join refused (two servers), with an admission naming smith"
	ADM=$($P admission -cluster-id $C -for "$(nodeid $S),$(field $S 'Node name'),10.79.20.81:8444,$(pin $S)" -member "$(nodeid $L),$(field $L 'Node name'),10.79.20.82:8444,$(pin $L)")
	on $S "sudo $SC join -cluster-id $C '$ADM'"; echo "exit=$?"
	waitfor $S 30 'Health: *ok'
	status $S | egrep '^Raft|^Members|member, voter'
	rootfiles $S
}

case4() {
	C=$(cid $S)
	h "4. star: join-request"
	on $T "sudo $SC join-request -address 10.79.20.83" | tee /tmp/8b1-req-t.txt; echo "exit=${PIPESTATUS[0]}"
	REQ=$(grep '^skep-join-request:' /tmp/8b1-req-t.txt | head -1)
	TP=$(pin $T); TID=$(nodeid $T); TC=$(cid $T)
	echo "star before: node $TID pin $TP cluster $TC"
	h "4a. admit, withdraw, then join: refused in the handshake"
	on $S "sudo $SC admit '$REQ'" | tee /tmp/8b1-adm-t1.txt; echo "exit=${PIPESTATUS[0]}"
	ADM1=$(grep '^skep-admission:' /tmp/8b1-adm-t1.txt | head -1)
	on $S "sudo $SC admit --withdraw star"; echo "exit=$?"
	sleep 2
	for x in $S $L; do echo "$(name $x) pending: $(status $x | sed -n '/^Pending/,/^Health/p' | grep -c star)"; done
	on $T "sudo $SC join -cluster-id $C '$ADM1'"; echo "exit=$?"
	echo "star after: node $(nodeid $T) pin $(pin $T) cluster $(cid $T)"; on $T "sudo $SC health"; echo "health-exit=$?"
	for x in $S $L; do echo "$(name $x) members: $(status $x | grep -c ' member, ')"; done
	h "4b. admit again, then join with a wrong member pin: nothing sent, state back"
	on $S "sudo $SC admit '$REQ'" | tee /tmp/8b1-adm-t2.txt; echo "exit=${PIPESTATUS[0]}"
	grep '^skep-admission:' /tmp/8b1-adm-t2.txt | head -1 > /tmp/8b1-adm-t2.line
	WRONG=$($P admission -cluster-id $C -for "$TID,star,10.79.20.83:8444,$TP" -member "$(nodeid $S),smith,10.79.20.81:8444,sha256:$(printf 'e%.0s' $(seq 64))" -member "$(nodeid $L),langrishe,10.79.20.82:8444,sha256:$(printf 'f%.0s' $(seq 64))")
	on $T "sudo $SC join -cluster-id $C '$WRONG'"; echo "exit=$?"
	echo "star after: node $(nodeid $T) pin $(pin $T) cluster $(cid $T)"; on $T "sudo $SC health"; echo "health-exit=$?"
	h "4c. duplicates refused: ID, name, address, pin (star is pending)"
	X=sha256:$(printf '2%.0s' $(seq 64))
	for r in "-id $TID -name dupid -address 10.79.20.97:8444 -pin $X" \
	         "-name star -address 10.79.20.97:8444 -pin $X" \
	         "-name dupaddr -address 10.79.20.83:8444 -pin $X" \
	         "-name duppin -address 10.79.20.97:8444 -pin $TP" \
	         "-name dupsmith -address 10.79.20.81:8444 -pin $X"; do
		REQD=$($P request $r); echo "-- request $r"; on $S "sudo $SC admit '$REQD'"; echo "exit=$?"
	done
}

case5() {
	C=$(cid $S)
	ADM2=$(cat /tmp/8b1-adm-t2.line)
	h "5. langrishe stopped, then star joins"
	on $L 'sudo service skep_cluster stop'; sleep 6
	on $S "sudo $SC health"; echo "smith health-exit=$?"
	on $T "sudo $SC join -cluster-id $C '$ADM2'"; echo "exit=$?"
	status $T | head -12; on $T "sudo $SC health"; echo "star health-exit=$?"
	on $S "sudo $SC health"; echo "smith health-exit=$?"
	h "5. langrishe started"
	on $L 'sudo service skep_cluster start'; waitfor $S 60 'Health: *ok'
	status $S | sed -n '/^Members/,$p'
}

case5b() {
	C=$(cid $S)
	h "5'. star joins with smith and langrishe running"
	ADM2=$(cat /tmp/8b1-adm-t2.line)
	status $S | sed -n '/^Pending/,/^Health/p'
	on $T "sudo $SC join -cluster-id $C '$ADM2'"; echo "exit=$?"
	waitfor $S 120 'star[^ ]* .*member, voter'
	for x in $S $L $T; do h "5'. state on $(name $x)"; status $x; memfile $x; done
	rootfiles $S $L $T
}

case6() {
	h "6. probe: unknown key and TLS 1.2 refused in the handshake"
	for x in $S $L $T; do $P tls -addr $x:8444 -version 1.3; $P tls -addr $x:8444 -version 1.2; done
	h "6. a member's key (smith's, read by the skep-cluster account) with a header claiming another node"
	SID=$(nodeid $S); LID=$(nodeid $L); TID=$(nodeid $T)
	for x in $L $T; do
		XP=$(pin $x)
		on $S "sudo -u skep-cluster $RP rpc -addr $x:8444 -server-pin $XP -key-dir /var/db/skep/cluster -claim-id $LID -claim-addr 10.79.20.82:8444 -op raft"
		on $S "sudo -u skep-cluster $RP rpc -addr $x:8444 -server-pin $XP -key-dir /var/db/skep/cluster -claim-id $TID -op status"
		echo "-- control: smith's key claiming smith, status"
		on $S "sudo -u skep-cluster $RP rpc -addr $x:8444 -server-pin $XP -key-dir /var/db/skep/cluster -claim-id $SID -op status"
	done
	h "6. a pending key: Raft RPC and status refused"
	rm -rf /tmp/8b1-probekey
	$P keygen -dir /tmp/8b1-probekey -name probe -address 10.79.20.80:8444 | tee /tmp/8b1-probekey.txt
	PREQ=$(grep '^skep-join-request:' /tmp/8b1-probekey.txt); PID=$(sed -n 's/^node ID //p' /tmp/8b1-probekey.txt)
	on $S "sudo $SC admit '$PREQ'" | head -3
	sleep 2
	for x in $S $L $T; do
		$P rpc -addr $x:8444 -server-pin $(pin $x) -key-dir /tmp/8b1-probekey -claim-id $PID -claim-addr 10.79.20.80:8444 -op raft
		$P rpc -addr $x:8444 -server-pin $(pin $x) -key-dir /tmp/8b1-probekey -claim-id $PID -op status
	done
	on $S "sudo $SC admit --withdraw probe"; echo "exit=$?"
	rm -rf /tmp/8b1-probekey /tmp/8b1-probekey.txt
}

case7() {
	h "7. stop all three, start all three"
	for x in $S $L $T; do on $x 'sudo service skep_cluster stop'; done
	for x in $S $L $T; do on $x 'sudo service skep_cluster start'; done
	t0=$(date +%s)
	waitfor $S 60 'Health: *ok' && echo "leader and health ok on smith $(( $(date +%s) - t0 )) s after the starts"
	for x in $S $L $T; do echo "$(name $x): $(status $x | egrep '^Raft:|^State machine:' | tr '\n' ' ')"; status $x | sed -n '/^Members/,/^Health/p'; done
	h "7. restart langrishe alone"
	on $L 'sudo service skep_cluster restart'; t0=$(date +%s)
	waitfor $L 60 'State machine: up to date' && echo "langrishe up to date $(( $(date +%s) - t0 )) s after its restart"
	status $L | egrep '^Raft:|^State machine:|^Health'
}

case8() {
	h "8. admit a request for a key nobody holds"
	REQ=$($P request -name ghost -address 10.79.20.96:8444 -pin sha256:$(openssl rand -hex 32))
	on $S "sudo $SC admit '$REQ'" | head -4
	for x in $S $L $T; do echo "$(name $x): $(status $x | sed -n '/^Pending/,/^Health/p' | grep ghost)"; done
	echo "waiting 31 minutes"; sleep 1860
	h "8. after 31 minutes"
	for x in $S $L $T; do echo "$(name $x) pending ghost: $(status $x | grep -c ghost)"; on $x "sudo grep -h 'pending expired' /var/log/daemon.log /var/log/messages 2>/dev/null | tail -1"; done
}

for c in "$@"; do
	case $c in
	pids) h "PIDs"; pids;;
	vid) for x in $S $L $T; do echo "$(name $x) vid=$(on $x 'sysctl -n hw.vmm.vmx.cap.virtual_interrupt_delivery 2>&1')"; done;;
	*) h "PIDs before case $c"; pids; "case$c"; h "PIDs after case $c"; pids;;
	esac
done
