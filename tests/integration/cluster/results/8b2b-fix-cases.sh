#!/bin/bash
# 8b-2b review-fix lab, cases 4.2 to 4.6 (job 4), driven from merrick over
# ssh as aba (sudo on the VMs). LD is the leader, R and X the two
# followers, read again before each case. Uses 8b2b-fix-lab.sh (next to
# this script) for the shared helpers.
# Usage: 8b2b-fix-cases.sh 4.2 | 4.3 | 4.4 | 4.5 | 4.6
set -u
here=$(cd "$(dirname "$0")" && pwd)
D="bash $here/8b2b-fix-lab.sh"
S=10.79.20.81; L=10.79.20.82; T=10.79.20.83
SC=/usr/local/sbin/skep-cluster
DD=/var/db/skep/cluster
CID=ff712cb0-58a7-41ba-87cf-4bff6b1579d0
on() { local h=$1; shift; ssh -o BatchMode=yes -o ConnectTimeout=10 aba@$h "$@"; }
h() { echo; echo "=== $(date -u +%H:%M:%SZ) $* ==="; }
name() { case $1 in $S) echo smith;; $L) echo langrishe;; $T) echo star;; esac; }
nodename() { on "$1" "sudo $SC status" | sed -n 's/^Node name: *//p'; }
pin() { on "$1" "sudo $SC status" | sed -n 's/^Node key pin: *//p'; }
roles() {
	LD=$($D leader)
	R=; X=
	for x in $S $L $T; do
		[ "$x" = "$LD" ] && continue
		if [ -z "$R" ]; then R=$x; else X=$x; fi
	done
	echo "roles: leader $(name "$LD") ($LD), R $(name "$R") ($R), X $(name "$X") ($X), term $($D term "$LD")"
	[ -n "$LD" ] && [ -n "$R" ] && [ -n "$X" ] || { echo "ROLES NOT FOUND"; exit 1; }
}
svc() { on "$1" "sudo timeout -f 120 service skep_cluster $2; echo service-$2-exit=\$?"; }
health3() { for x in $S $L $T; do echo "$(name $x): $(on $x "sudo $SC health | head -1; echo exit=\$?" | tr '\n' ' ')"; done; }
pins3() { for x in $S $L $T; do echo "-- on $(name $x):"; on $x "sudo $SC status" | sed -n '/^Members/,/^Clocks/p' | grep 'member,'; done; }
verify3() { for x in $S $L $T; do echo "$(name $x): $(on $x "sudo $SC audit verify >/dev/null 2>&1; echo verify-exit=\$?")"; done; }
files() { on "$1" "sudo ls $DD"; }

case ${1:-} in
4.2)
	h "4.2 rotate R's key with all up"; roles
	old=$(pin "$R"); echo "R's pin before: $old"
	on "$R" "sudo $SC key rotate -wait 2m"; echo "rotate-exit=$?"
	new=$(pin "$R"); echo "R's pin after: $new"
	pins3
	for x in $S $L $T; do
		c=$(on $x "sudo $SC status" | grep -c "$new"); echo "$(name $x): new pin listed $c time(s); old pin listed $(on $x "sudo $SC status" | grep -c "$old")"
	done
	$D wait3; health3; verify3
	echo "R's data directory: $(files "$R" | tr '\n' ' ')" ;;
4.3)
	h "4.3 stop X; rotate on R waits; abort; start X"; roles
	old=$(pin "$R"); echo "R's pin before: $old"
	svc "$X" stop
	on "$R" "sudo $SC key rotate -wait 10s"; echo "rotate-exit=$? (3 expected)"
	echo "-- status on R during the rotation:"; on "$R" "sudo $SC status" | egrep 'next pin|Rotation|Health|key_rotation'
	on "$R" "sudo $SC key rotate --abort"; echo "abort-exit=$? (0 expected)"
	sleep 3
	echo "R's pin after the abort: $(pin "$R") (before $old)"
	for x in $LD $R; do echo "-- on $(name $x):"; on $x "sudo $SC status" | grep "$(nodename "$R")" ; done
	echo "R's data directory: $(files "$R" | tr '\n' ' ')"
	svc "$X" start
	$D wait3; health3
	echo "-- on X: $(on "$X" "sudo $SC status" | grep "$(nodename "$R")")" ;;
4.4)
	h "4.4 remove X with all up, leave --wipe, join-request, admit, join"; roles
	xn=$(nodename "$X"); xip=$X
	on "$LD" "sudo $SC remove $xn"; echo "remove-exit=$?"
	i=0; while [ $i -lt 60 ]; do on "$X" "sudo $SC status" | grep -q '^REMOVED' && break; sleep 2; i=$((i+2)); done
	on "$X" "sudo $SC status" | egrep '^REMOVED|^Raft' ; echo "(waited ${i}s)"
	on "$X" "sudo $SC leave --wipe -cluster-id $CID"; echo "wipe-exit=$?"
	req=$(on "$X" "sudo $SC join-request -address $xip" | tee /dev/stderr | grep -o 'skep-join-request:[^ ]*' | head -1)
	echo "X's own pin: $(pin "$X")"
	adm=$(on "$LD" "sudo $SC admit $req" | tee /dev/stderr | grep -o 'skep-admission:[^ ]*' | head -1)
	[ -n "$adm" ] || { echo "NO ADMISSION"; exit 1; }
	on "$X" "sudo $SC join -cluster-id $CID $adm"; echo "join-exit=$?"
	$D wait3; health3
	echo "-- X's audit after its last join mark:"
	tmp=$(mktemp); on "$X" "sudo sh -c 'cat $DD/audit/audit-*.jsonl'" > "$tmp"
	python3 -I - "$tmp" <<'PY'
import json, sys
recs = [json.loads(l) for l in open(sys.argv[1])]
marks = [i for i, r in enumerate(recs) if r["kind"] == "node.join.started"]
i = marks[-1]
op = recs[i]["details"]["join_op"]
print("join mark: seq", recs[i]["seq"], "join_op", op)
for r in recs[i + 1:i + 5]:
    print("  seq", r["seq"], r["kind"], r.get("entry_type", ""), r.get("op_id", ""), "index", r.get("index", ""))
n = recs[i + 1]
ok = n["kind"] == "entry" and n.get("entry_type") == "member.join" and n.get("op_id") == op
print("first record after the mark is this join's member.join:", "YES" if ok else "NO")
PY
	rm -f "$tmp"
	verify3 ;;
4.5)
	h "4.5 the broken-audit procedure on R"; roles
	svc "$R" stop
	f=$DD/audit/audit-00000001.jsonl
	b=$(on "$R" "sudo dd if=$f bs=1 skip=30 count=1 2>/dev/null"); nb=0; [ "$b" = 0 ] && nb=1
	echo "byte at offset 30: '$b' -> '$nb'"
	on "$R" "printf $nb | sudo dd of=$f bs=1 seek=30 count=1 conv=notrunc 2>/dev/null; sudo ls -l $f"
	on "$R" "sudo $SC audit verify; echo verify-exit=\$?"
	svc "$R" start
	sleep 8
	echo "-- skep-cluster processes on R: [$(on "$R" "pgrep -x skep-cluster | tr '\n' ' '")]"
	echo "-- what the start printed (daemon.log):"
	on "$R" "sudo grep skep-cluster /var/log/daemon.log | grep -i 'audit' | tail -1"
	echo "-- following the procedure as printed:"
	svc "$R" stop
	on "$R" "sudo $SC audit verify; echo verify-exit=\$?"
	moved=$DD/audit.broken.$(date -u +%Y%m%dT%H%M%SZ)
	on "$R" "sudo mv $DD/audit $moved; sudo ls -ld $moved"
	svc "$R" start
	$D wait3; health3
	on "$R" "sudo $SC audit verify; echo verify-exit=\$?"
	on "$R" "sudo $SC audit verify -dir $moved; echo verify-moved-exit=\$?"
	echo "R's data directory: $(files "$R" | tr '\n' ' ')" ;;
4.6)
	h "4.6 end state"
	for x in $S $L $T; do
		echo "-- $(name $x)"
		on $x "$SC version; pkg query '%n %v' skep; sudo $SC status | egrep '^(Cluster ID|Node key pin|Raft|State machine|Build|Health)'; sudo $SC health | head -1"
		on $x "sudo $SC status" | sed -n '/^Members/,/^Clocks/p' | grep 'member,'
		echo "pending/rotation/join files: [$(files $x | egrep 'join|rotate|standalone|next|wipe|removed' | tr '\n' ' ')]"
		echo "data directory: $(files $x | tr '\n' ' ')"
		echo "jails=$(on $x 'jls | tail -n +2 | wc -l' | tr -d ' ')"
	done
	$D pids; $D rootcheck; verify3 ;;
*) echo "usage: $0 4.2|4.3|4.4|4.5|4.6" >&2; exit 2 ;;
esac
