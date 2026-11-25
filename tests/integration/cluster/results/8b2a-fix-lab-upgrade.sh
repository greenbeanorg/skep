#!/bin/sh
# 8b-2a fix round, job 3.1: upgrade one host to the branch's package and
# restart skep_cluster by hand (the hosts already have the state/ layout).
# Run as aba (sudo) on the host.
# Usage: 8b2a-fix-lab-upgrade.sh DIR CHECKARGS...   (DIR holds pkg/)
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
h "service skep_cluster restart (by hand)"
sudo service skep_cluster restart; echo "exit=$?"
sleep 2
h "after"
/usr/local/sbin/skep-agent -version; /usr/local/sbin/skep-cluster version
pids; sha256 -q /etc/rc.conf; sha256 -q /boot/loader.conf
echo "vid=$(sysctl -n hw.vmm.vmx.cap.virtual_interrupt_delivery 2>&1)"
echo "root-owned in $DD: [$(sudo find $DD -user root | tr '\n' ' ')]"
