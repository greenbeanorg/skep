#!/bin/sh
# failopen.sh PORT RESOURCE_ID CASE : break the port's filter as host root,
# then wait for the agent's reaction in its audit log (ADR 0011 decision 5).
set -u
P=$1; RID=$2; CASE=$3
DB=/var/db/skep/agent/agent.db
q() { sudo sqlite3 -readonly -cmd '.timeout 5000' $DB "$1"; }
mark=$(q "SELECT max(id) FROM audit_log")
bpf=$(sudo ngctl show $P: | awk '$1=="lower"{print $4}')
echo "==== $CASE on $P (filter node [$bpf]) $(date -u +%T)"
t0=$(date +%s)
case $CASE in
rmhook-lower) sudo ngctl rmhook $P: lower ;;
shutdown-node) sudo ngctl shutdown skf_$P: ;;
pass-all) sudo ngctl msg [$bpf]: setprogram '{ thisHook="jail" ifMatch="bridge" ifNotMatch="" bpf_prog_len=1 bpf_prog=[ { code=6 jt=0 jf=0 k=4294967295 } ] }' ;;
rmhook-upper) sudo ngctl rmhook $P: upper ;;
foreign-lower)
	# Remove lower and hold it with a socket node (nghook), so the
	# reinstall's mkpeer cannot succeed: the port must be detached.
	sudo ngctl rmhook $P: lower
	(sleep 60 | sudo nghook $P: lower > /dev/null 2>&1 &)
	sleep 0.5 ;;
esac
echo "  after the change:"; sudo ngctl show $P: | sed 's/^/    /'
want=port_filter.restored
[ "$CASE" = foreign-lower ] && want=port_filter.detached
i=0
while [ $i -lt 60 ]; do
	n=$(q "SELECT count(*) FROM audit_log WHERE id > $mark AND action = '$want' AND resource_id = '$RID'")
	[ "$n" -gt 0 ] && break
	sleep 0.5; i=$((i+1))
done
t1=$(date +%s)
echo "  reaction ($want) after about $((t1-t0)) s"
q "SELECT occurred_at, action, outcome, json_extract(details, '$.problem'), json_extract(details, '$.consecutive_failures'), json_extract(details, '$.reason') FROM audit_log WHERE id > $mark AND resource_id = '$RID' ORDER BY id" | sed 's/^/  audit: /'
echo "  after the reaction:"; sudo ngctl show $P: 2>&1 | sed 's/^/    /'
ifconfig bridge1 | grep -q "member: $P " && echo "  $P is a member of bridge1" || echo "  $P is NOT a member of bridge1"
