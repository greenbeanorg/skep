#!/bin/sh
# 8a lab test on smith, part 2: restart, modes, socket callers, listen, TLS, end state.
set -u
D="$1"
P="$D/cluster-probe"
S=/var/run/skep-cluster/cluster.sock
h() { echo; echo "=== $* ==="; }
ids() { sudo /usr/local/sbin/skep-cluster status | egrep 'Node ID|Node key pin|Cluster ID'; }
h "identity before a restart"
ids > /tmp/m8a-before.$$; cat /tmp/m8a-before.$$
sudo service skep_cluster status
h "restart"
sudo service skep_cluster restart; echo "exit=$?"; sleep 3
sudo service skep_cluster status
ids > /tmp/m8a-after.$$; cat /tmp/m8a-after.$$
if cmp -s /tmp/m8a-before.$$ /tmp/m8a-after.$$; then echo "SAME node ID, pin and cluster ID after the restart"; else echo "DIFFERENT after the restart"; fi
rm -f /tmp/m8a-before.$$ /tmp/m8a-after.$$
h "node ID in cluster.json"
sudo cat /usr/local/etc/skep/cluster.json
h "owners and modes"
stat -f '%Sp %Su:%Sg %N' /var/db/skep/cluster /usr/local/etc/skep/cluster.json /var/run/skep-cluster /var/run/skep-cluster/cluster.sock /var/run/skep
sudo sh -c "stat -f '%Sp %Su:%Sg %N' /var/db/skep/cluster/* /var/db/skep/cluster/snapshots/* 2>/dev/null"
h "root-owned files under /var/db/skep/cluster (expected: none)"
sudo find /var/db/skep/cluster -user root; echo "find-exit=$?"
sudo find /var/db/skep/cluster | wc -l
h "socket: root"
sudo "$P" get -socket $S -path /cluster/v1/health | head -3
sudo "$P" get -socket $S -path /cluster/v1/status | head -2
sudo "$P" get -socket $S -path /cluster/v1/raft/configuration
h "socket: skep (uid 409)"
sudo -u skep "$P" get -socket $S -path /cluster/v1/health | head -2
sudo -u skep "$P" get -socket $S -path /cluster/v1/status | head -2
sudo -u skep "$P" get -socket $S -path /cluster/v1/raft/configuration
sudo -u skep /usr/local/sbin/skep-cluster health; echo "exit=$?"
h "socket: the skep-cluster account itself (passes file permissions, not an allowed peer)"
sudo -u skep-cluster "$P" get -socket $S -path /cluster/v1/health
sudo -u skep-cluster "$P" get -socket $S -path /cluster/v1/status
h "socket: aba (neither)"
"$P" get -socket $S -path /cluster/v1/health
/usr/local/sbin/skep-cluster status; echo "exit=$?"
h "socket: nobody"
sudo -u nobody "$P" get -socket $S -path /cluster/v1/status
h "agent socket as the skep-cluster account"
sudo -u skep-cluster "$P" get -socket /var/run/skep/agent.sock -path /internal/v1/health -actor probe
sudo -u skep-cluster "$P" get -socket /var/run/skep/agent.sock -path /internal/v1/system -actor probe
echo "-- for comparison, as skep:"
sudo -u skep "$P" get -socket /var/run/skep/agent.sock -path /internal/v1/health -actor probe | head -2
h "node ID from the agent (root, read-only system operation)"
sudo "$P" get -socket /var/run/skep/agent.sock -path /internal/v1/system -actor probe | tr ',' '\n' | grep node_id
h "service log: refused callers"
sudo grep skep-cluster /var/log/daemon.log | grep -c 'refused'
sudo grep skep-cluster /var/log/daemon.log | grep 'refused' | tail -4 | cut -c1-260
h "listen as a user, and with bad addresses (all refused)"
/usr/local/sbin/skep-cluster listen -address 10.79.20.81; echo "exit=$?"
sudo /usr/local/sbin/skep-cluster listen; echo "exit=$?"
sudo /usr/local/sbin/skep-cluster listen -address 0.0.0.0; echo "exit=$?"
sudo /usr/local/sbin/skep-cluster listen -address 10.79.20.99; echo "exit=$?"
sudo cat /usr/local/etc/skep/cluster.json
h "listen -address 10.79.20.81"
sudo /usr/local/sbin/skep-cluster listen -address 10.79.20.81; echo "exit=$?"
sudo cat /usr/local/etc/skep/cluster.json
stat -f '%Sp %Su:%Sg %N' /usr/local/etc/skep/cluster.json
h "what listens"
sockstat -l | egrep '8444|8445'
echo "-- every socket of the skep-cluster account:"
sudo sockstat -l | awk 'NR==1 || $1 ~ /^skep-clu/'
h "status after listen"
sudo /usr/local/sbin/skep-cluster status
sudo "$P" get -socket $S -path /cluster/v1/raft/configuration
h "TLS 1.2 handshake"
"$P" tls -addr 10.79.20.81:8444 -version 1.2
h "TLS 1.3 handshake with an unknown key"
"$P" tls -addr 10.79.20.81:8444 -version 1.3
h "openssl s_client, TLS 1.2 and TLS 1.3 without a certificate"
openssl s_client -connect 10.79.20.81:8444 -tls1_2 </dev/null 2>&1 | egrep -i 'alert|error|Protocol|Cipher is' | head -5
openssl s_client -connect 10.79.20.81:8444 -tls1_3 </dev/null 2>&1 | egrep -i 'alert|error|Protocol|Cipher is' | head -5
h "not reachable on other addresses"
"$P" tls -addr 127.0.0.1:8444 -version 1.3
h "listener log"
sudo grep skep-cluster /var/log/daemon.log | grep 'node listener' | tail -5 | cut -c1-300
h "root-owned files under /var/db/skep/cluster after listen (expected: none)"
sudo find /var/db/skep/cluster -user root; echo "find-exit=$?"
h "end state"
date -u +%Y-%m-%dT%H:%M:%SZ
sudo service skep_agent status; sudo service skep_api status; sudo service skep_cluster status
fetch -qo - --no-verify-peer --no-verify-hostname https://10.79.20.81:8443/api/v1/health; echo
sudo /usr/local/sbin/skep-cluster health; echo "exit=$?"
sysctl hw.vmm.vmx.cap.virtual_interrupt_delivery
grep -n skep /etc/rc.conf; sha256 -q /boot/loader.conf
sudo jls -N name | wc -l; ls /dev/vmm 2>&1
sockstat -l | egrep 'skep'
pkg query '%n %v' skep; /usr/local/sbin/skep-cluster version
