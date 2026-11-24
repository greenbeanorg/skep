#!/bin/sh
# 8a lab test on smith, part 1: package, upgrade, init, start. Run as aba (sudo).
set -u
D="$1"   # directory with pkg/ and cluster-probe
h() { echo; echo "=== $* ==="; }
cd "$D/pkg" || exit 9
h "before"
date -u +%Y-%m-%dT%H:%M:%SZ
sha256 -q /etc/rc.conf; sha256 -q /boot/loader.conf
sudo service skep_agent status; sudo service skep_api status
id skep; id skep-cluster 2>&1
h "pkg create"
mkdir -p out
pkg create -M +MANIFEST -p plist -r stage -o out || exit 1
pkg info -F out/skep-0.1.0.pkg | egrep 'Name|Version|Architecture'
pkg info -l -F out/skep-0.1.0.pkg
h "pkg install -y -f"
sudo pkg install -y -f ./out/skep-0.1.0.pkg; echo "exit=$?"
id skep-cluster
sha256 -q /etc/rc.conf
h "skep-setup upgrade -yes -keep 50"
sudo skep-setup upgrade -yes -keep 50; echo "exit=$?"
h "after upgrade"
/usr/local/sbin/skep-agent -version; /usr/local/sbin/skep-cluster version
sudo service skep_agent status; sudo service skep_api status
fetch -qo - --no-verify-peer --no-verify-hostname https://10.79.20.81:8443/api/v1/health; echo
sudo service skep_cluster status 2>&1; ls -la /var/db/skep/ | grep cluster; ls /usr/local/etc/skep/
h "init as a user (must be refused)"
/usr/local/sbin/skep-cluster init; echo "exit=$?"
h "service start before init (must be refused)"
sudo service skep_cluster onestart 2>&1; echo "exit=$?"
h "skep-cluster init"
sudo /usr/local/sbin/skep-cluster init; echo "exit=$?"
h "second init (must be refused)"
sudo /usr/local/sbin/skep-cluster init; echo "exit=$?"
h "enable and start"
sudo sysrc skep_cluster_enable=YES
sudo service skep_cluster start; echo "exit=$?"
sleep 3
sudo service skep_cluster status
h "status (root)"
sudo /usr/local/sbin/skep-cluster status; echo "exit=$?"
h "nothing listens on 8444 after init"
sockstat -l | egrep '8444|8445' ; echo "grep-exit=$? (1 = nothing)"
echo "-- every socket of the skep-cluster account:"
sudo sockstat -l | awk 'NR==1 || $1 ~ /^skep-clu/'
