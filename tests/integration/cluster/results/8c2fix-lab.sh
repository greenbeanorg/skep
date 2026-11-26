#!/bin/bash
# 8c-2 review-fix lab: the fix build on smith (S), langrishe (L), star (T)
# and blazanov (B), from merrick over ssh as aba (sudo on the VMs). Uses
# 8c2-labB.sh for the three-node cluster and ../../setup/results/8c2-labA.sh
# for blazanov; API calls go through 8c2-api.py (pinned TLS; passwords
# never printed). Each case is one command, run by hand and not in a loop.
# Raw logs stay in ~/skep-test-logs on merrick.
#
# Usage: 8c2fix-lab.sh L2a HOST          on HOST's own API remove HOST (HOST must be the leader)
#        8c2fix-lab.sh L2b HOST          on HOST: leave --wipe -cluster-id ID (the old cluster's), join-request
#        8c2fix-lab.sh L2c MEMBER HOST   admit HOST's join request on MEMBER's API; join on HOST
#        8c2fix-lab.sh L3 HOST           skep-setup remove -plan on HOST
#        8c2fix-lab.sh leader            the leader's name
set -u
here=$(cd "$(dirname "$0")" && pwd)
B="$here/8c2-labB.sh"
API="$here/8c2-api.py"
S=10.79.20.81; L=10.79.20.82; T=10.79.20.83
SC=/usr/local/sbin/skep-cluster
PW=~/skep-lab-creds/smith-admin
: "${LAB8C2_STATE:?set LAB8C2_STATE to a 0600 file path in the scratchpad}"
export LAB8C2_STATE
W=$(dirname "$LAB8C2_STATE")
ip() { case $1 in smith) echo $S;; langrishe) echo $L;; star) echo $T;; *) echo "unknown host $1" >&2; exit 2;; esac; }
on() { local h=$1; shift; ssh -o BatchMode=yes -o ConnectTimeout=10 aba@"$(ip "$h")" "$@"; }
h() { echo; echo "=== $(date -u +%H:%M:%SZ) $* ==="; }
leader() { for n in smith langrishe star; do
	on $n "sudo $SC status" 2>/dev/null | grep -q '^Raft: *leader' && { echo $n; return; }; done; echo none; }

case ${1:-} in
leader) leader ;;
L2a)
	x=$2
	[ "$(leader)" = "$x" ] || { echo "$x is not the leader ($(leader)); stop"; exit 3; }
	cid=$(bash "$B" clusterid)
	echo "$cid" > "$W/L2-cluster-id"
	h "L2a: on $x's own API remove $x (cluster $cid)"
	python3 "$API" login "$x" admin "$PW"
	python3 "$API" req "$x" admin POST "/cluster/members/$(on $x "sudo $SC status -json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"]["node_name"])')/remove" "{\"cluster_id\":\"$cid\"}"
	;;
L2b)
	x=$2
	cid=$(cat "$W/L2-cluster-id")
	h "L2b: $x: skep-cluster leave --wipe -cluster-id $cid"
	on $x "sudo $SC leave --wipe -cluster-id $cid"; echo "exit=$?"
	h "L2b: $x: skep-cluster join-request -address $(ip $x)"
	on $x "sudo $SC join-request -address $(ip $x)" > "$W/L2-joinreq.out"; echo "exit=$?"
	grep -o 'skep-join-request:[A-Za-z0-9:_=-]*' "$W/L2-joinreq.out" | head -1 > "$W/L2-request"
	echo "request: $(cut -c1-40 "$W/L2-request")..."
	;;
L2c)
	m=$2; x=$3
	h "L2c: admit $x on $m's API"
	python3 "$API" login "$m" admin "$PW"
	python3 "$API" admit "$m" admin "$W/L2-request" "$W/L2-admission"
	[ -s "$W/L2-admission" ] || { echo "no admission; stop"; exit 3; }
	h "L2c: $x: skep-cluster join (the admission is not printed)"
	on $x "sudo $SC join $(cat "$W/L2-admission")" | grep -v 'skep-admission:'; echo "exit=${PIPESTATUS[0]}"
	bash "$B" wait3
	;;
L3)
	x=$2
	h "L3: $x: skep-setup remove -plan"
	on $x "sudo skep-setup remove -plan"; echo "exit=$?"
	;;
*) sed -n '2,15p' "$0"; exit 2 ;;
esac
