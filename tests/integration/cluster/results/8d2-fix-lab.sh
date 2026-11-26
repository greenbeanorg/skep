#!/bin/bash
# 8d-2 fix round, job 3: the lab check of F1 (the join clears the agent's
# drain once the service is back after the join call, not when the command
# returns). Run from the repo root on merrick after deploy/upgrade with
# 8d2-labB.sh. One pass, steps 1-5 of the prompt; every command's output is
# kept. Times on blazanov are read on blazanov's clock.
set -u
LAB=tests/integration/cluster/results/8d2-labB.sh
API=tests/integration/cluster/results/8d2-api.py
S=10.79.20.81; L_=10.79.20.82; T=10.79.20.83; B=10.79.20.84
SC=/usr/local/sbin/skep-cluster
on() { local h=$1; shift; ssh -o BatchMode=yes -o ConnectTimeout=10 aba@$h "$@"; }
h() { echo; echo "=== $(date -u +%H:%M:%SZ) $* ==="; }
ip() { case $1 in smith) echo $S;; langrishe) echo $L_;; star) echo $T;; esac; }
OUT=${SCRATCH:?set SCRATCH to a scratch directory}

h "1. roles"
$LAB roles | tee $OUT/roles
LN=$(awk '$3=="leader"{print $1}' $OUT/roles); LIP=$(ip $LN)
XN=$(awk '$3=="follower"{print $1}' $OUT/roles | head -1); XIP=$(ip $XN)
RN=$(awk '$3=="follower"{print $1}' $OUT/roles | tail -1)
echo "L=$LN X=$XN R=$RN"
[ -n "$LN" ] && [ -n "$XN" ] && [ "$XN" != "$RN" ] || { echo "roles not read"; exit 1; }
CID=$($LAB clusterid); echo "cluster $CID"
echo "blazanov pin before: $(on $B "sudo $SC status" | sed -n 's/^Node key pin: *//p')"
echo "blazanov cluster before: $(on $B "sudo $SC status" | sed -n 's/^Cluster ID: *//p')"
$LAB svc $XIP skep_cluster stop

h "2. join-request on blazanov, admit on $LN"
on $B "sudo $SC join-request -address 10.79.20.84" > $OUT/req.txt 2>&1; echo "exit=$?"
cat $OUT/req.txt
REQ=$(grep '^skep-join-request:' $OUT/req.txt | head -1)
on $LIP "sudo $SC admit '$REQ'" > $OUT/adm.txt 2>&1; echo "exit=$?"
grep -v '^skep-admission:\|skep-cluster join skep-admission' $OUT/adm.txt
ADM=$(grep '^skep-admission:' $OUT/adm.txt | head -1)
[ -n "$REQ" ] && [ -n "$ADM" ] || { echo "no request or admission line"; exit 1; }
h "join -cluster-id ... -wait 45s on blazanov (background, output timestamped on blazanov)"
timeout 150 ssh -o BatchMode=yes aba@$B "{ sudo $SC join -cluster-id $CID -wait 45s '$ADM'; echo \"exit=\$? end=\$(date -u +%H:%M:%S)\"; } 2>&1 | while IFS= read -r l; do echo \"\$(date -u +%H:%M:%S) \$l\"; done" > $OUT/join.txt 2>&1 &
JPID=$!
echo "join pid $JPID"

h "3. drain about 15 s after the accepted line"
i=0; until grep -q 'The cluster accepted\|join failed\|outcome is unknown\|^.* exit=' $OUT/join.txt || [ $i -ge 90 ]; do sleep 1; i=$((i+1)); done
grep 'The cluster accepted\|exit=' $OUT/join.txt
sleep 15
echo "drain read at $(on $B 'date -u +%H:%M:%S'): $($LAB drain $B)"
if kill -0 $JPID 2>/dev/null; then echo "the join command is still running"; else echo "the join command has ended already"; fi
wait $JPID; echo "ssh exit $?"
echo "--- join output"; cat $OUT/join.txt
echo "--- the agent's drain audit (last 4)"
on $B "sudo skep-agent audit export --unsealed 2>/dev/null" | grep 'node\.drain' | tail -4
echo "--- blazanov status"; on $B "sudo $SC status" | grep -E '^(Cluster ID|Node key pin|Raft|Health)|blazanov'
echo "--- $LN status"; on $LIP "sudo $SC status" | grep -E '^(Raft|Health)|blazanov|  [a-z]+: '

h "4. remove blazanov on $LN, join --abandon on blazanov"
$LAB sc $LIP remove blazanov.greenbean.org
$LAB sc $B join --abandon
on $B "sudo $SC status" | grep -E '^(Cluster ID|Node key pin|Raft|Listener|Health)'
echo "users: $(on $B 'sudo skep-api admin list' | tr '\n' ';')"
python3 $API login blazanov admin ~/skep-lab-creds/blazanov-admin

h "5. start skep_cluster on $XN"
$LAB svc $XIP skep_cluster start
$LAB wait3
for x in $S $L_ $T; do echo "$x: $(on $x "sudo $SC status" | grep -E '^Health' ; on $x "sudo $SC health" | tr '\n' ' ')"; done
