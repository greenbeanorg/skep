#!/bin/sh
# B1 and the pre-reboot half of B2 on smith. Run as root.
set -u
S=/var/tmp/skep-b2/skep-setup
D=/var/tmp/skep-b2
plan() { "$S" plan -mgmt vtnet0 -bridge bridge9 2>&1; }
item() { awk '/bridge9 vlanfilter/{p=1;print;next} p&&/^  (ok|warn|manual|add|refuse|conflict) /{p=0} p'; }
echo "before: rc.conf $(sha256 -q /etc/rc.conf), ifconfig -l: $(ifconfig -l)"
echo "before: $(ifconfig vtnet1 | head -1)"
cp -p /etc/rc.conf "$D/rc.conf.orig"
ifconfig bridge9 create || exit 1
ifconfig bridge9 addm vtnet1 up || exit 1
EP=$(ifconfig epair create) || exit 1
ifconfig "$EP" up
ifconfig bridge9 addm "$EP" || exit 1
echo; echo "=== B1: bridge9 with live guest port $EP"
plan | item
if plan | item | grep -q '\$ '; then echo "B1 FAILED: steps printed"; else echo "B1: no steps printed"; fi
ifconfig "$EP" destroy; echo "destroyed $EP"
echo; echo "=== B2: plan with the uplink only"
plan | item | tee "$D/item"
sed -n 's/^ *\$ //p' "$D/item" > "$D/steps"
echo; echo "=== B2: executing the printed steps"
while IFS= read -r step; do
  echo "\$ $step"; sh -c "$step"; rc=$?; echo "  rc=$rc"
  [ "$rc" -eq 0 ] || { echo "STEP FAILED"; exit 2; }
done < "$D/steps"
echo; echo "=== B2: live state"
ifconfig bridge9 | grep -E 'flags=|bridge flags|member:|untagged|vlan' 
grep -E '^(cloned_interfaces|ifconfig_bridge9)=' /etc/rc.conf
echo; echo "=== B2: plan after the steps"
plan | grep -E 'bridge9' 
