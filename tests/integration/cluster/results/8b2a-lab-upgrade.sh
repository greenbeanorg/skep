#!/bin/sh
# 8b-2a lab, job 4.1: upgrade one host to the branch's package, as the 8b-1
# fix round did, then show that the new build refuses the old data layout
# and move it by hand (decision D1). Run as aba (sudo) on the host.
# Usage: 8b2a-lab-upgrade.sh DIR CHECKARGS...   (DIR holds pkg/)
set -u
D="$1"; shift
DD=/var/db/skep/cluster
h() { echo; echo "=== $(date -u +%H:%M:%SZ) $* ==="; }
pids() { for s in skep-agent skep-api skep-cluster; do printf "%s=%s " $s "$(pgrep -x $s | tr '\n' ' ')"; done; echo; }
cd "$D/pkg" || exit 9
h "before"; hostname
sha256 -q /etc/rc.conf; sha256 -q /boot/loader.conf
echo "vid=$(sysctl -n hw.vmm.vmx.cap.virtual_interrupt_delivery 2>&1)"
pids; pkg query '%n %v' skep; /usr/local/sbin/skep-agent -version
h "staged binaries"; sha256 -r stage/usr/local/sbin/* | sed 's,stage/,,'
h "pkg create"
rm -rf out; mkdir -p out
pkg create -M +MANIFEST -p plist -r stage -o out || exit 1
pkg info -F out/skep-0.1.0.pkg | egrep 'Name|Version|Architecture'
h "pkg install -y -f ./out/skep-0.1.0.pkg"
sudo pkg install -y -f ./out/skep-0.1.0.pkg; echo "exit=$?"
h "skep-setup upgrade -yes -keep 50"
sudo skep-setup upgrade -yes -keep 50; echo "exit=$?"
h "skep-setup check $*  (exit 1 expected for known items)"
sudo skep-setup check "$@"; echo "exit=$?"
h "after the upgrade"
/usr/local/sbin/skep-agent -version; /usr/local/sbin/skep-cluster version
pids
h "stop skep_cluster (still the old build's process)"
sudo service skep_cluster stop; echo "exit=$?"
sudo ls -la $DD
h "the new build refuses the old layout (run once in the foreground as the service account)"
sudo -u skep-cluster /usr/local/sbin/skep-cluster -log-format text 2>&1 | tail -3; echo "exit=$?"
h "hand move as the service account: state/ (0700) and the three items into it"
sudo -u skep-cluster sh -c "cd $DD && mkdir -m 700 state && mv raft.db snapshots membership.json state/ && sync"; echo "exit=$?"
sudo ls -la $DD $DD/state
h "start skep_cluster by hand"
sudo service skep_cluster start; echo "exit=$?"
sleep 2
pids; sha256 -q /etc/rc.conf; sha256 -q /boot/loader.conf
echo "vid=$(sysctl -n hw.vmm.vmx.cap.virtual_interrupt_delivery 2>&1)"
