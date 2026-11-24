#!/bin/sh
# 8a fix round on smith: install the branch's package, upgrade, restart
# skep_cluster, then the node.crt test: stop skep_cluster, empty node.crt
# as the skep-cluster account, start it. Run as aba (sudo).
set -u
D="$1"   # directory with pkg/ (stage, +MANIFEST, plist)
C=/var/db/skep/cluster
h() { echo; echo "=== $* ==="; }
ident() { sudo /usr/local/sbin/skep-cluster status | egrep '^(Node ID|Cluster ID|Node key pin|Certificate|Raft|Listener|Build|Health)'; }
cd "$D/pkg" || exit 9
h "before"
date -u +%Y-%m-%dT%H:%M:%SZ
sha256 -q /etc/rc.conf; sha256 -q /boot/loader.conf
sysctl hw.vmm.vmx.cap.virtual_interrupt_delivery
sudo service skep_agent status; sudo service skep_api status; sudo service skep_cluster status
ident
h "pkg create"
mkdir -p out
pkg create -M +MANIFEST -p plist -r stage -o out || exit 1
pkg info -F out/skep-0.1.0.pkg | egrep 'Name|Version|Architecture'
h "pkg install -y -f"
sudo pkg install -y -f ./out/skep-0.1.0.pkg; echo "exit=$?"
h "skep-setup upgrade -yes -keep 50"
sudo skep-setup upgrade -yes -keep 50; rc=$?; echo "exit=$rc"
[ "$rc" = 0 ] || { echo "GATE FAILED: upgrade exit $rc"; exit 1; }
for i in 1 2 3 4 5 6 7 8 9 10; do
	out=$(fetch -qo - --no-verify-peer --no-verify-hostname https://10.79.20.81:8443/api/v1/health 2>&1) && break
	sleep 2
done
echo "$out"
case "$out" in *'"status":"ok"'*) ;; *) echo "GATE FAILED: /api/v1/health"; exit 1;; esac
/usr/local/sbin/skep-agent -version; /usr/local/sbin/skep-cluster version
h "restart skep_cluster (skep-setup does not manage it yet)"
sudo service skep_cluster restart; echo "exit=$?"
sleep 3
sudo service skep_cluster status
ident
h "node.crt test: stop, empty node.crt as skep-cluster, start"
sudo service skep_cluster stop; echo "stop exit=$?"
sudo ls -la $C/node.crt; sudo sha256 -q $C/node.crt
sudo -u skep-cluster sh -c ": > $C/node.crt"; echo "truncate exit=$?"
sudo ls -la $C/node.crt
mark=$(date -u +%Y-%m-%dT%H:%M:%S)
sudo service skep_cluster start; echo "start exit=$?"
sleep 4
sudo service skep_cluster status
ident
sudo /usr/local/sbin/skep-cluster health; echo "health exit=$?"
echo "-- node.crt after the start:"
sudo ls -la $C/node.crt; sudo sha256 -q $C/node.crt
echo "-- the warning (syslog, tag skep-cluster, since $mark UTC):"
sudo grep -h 'skep-cluster' /var/log/messages /var/log/daemon.log 2>/dev/null | grep 'new one was issued' | tail -2
echo "-- what listens:"
sockstat -l -P tcp | egrep '8443|8444|8445'
echo "-- root-owned files under $C (expect none):"
sudo find $C -user root
sudo ls -la $C
h "after"
sudo service skep_agent status; sudo service skep_api status
fetch -qo - --no-verify-peer --no-verify-hostname https://10.79.20.81:8443/api/v1/health; echo
sysctl hw.vmm.vmx.cap.virtual_interrupt_delivery
sha256 -q /etc/rc.conf; sha256 -q /boot/loader.conf
jls; ls /dev/vmm 2>&1
date -u +%Y-%m-%dT%H:%M:%SZ
