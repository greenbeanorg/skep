#!/bin/sh
# 8b-1 lab, job 4: upgrade a host that has skep installed (smith, langrishe)
# from the branch's package, as the 8a fix round did. Run as aba (sudo).
# Usage: 8b1-lab-upgrade.sh DIR CHECKARGS...   (DIR holds pkg/ and cluster-probe)
set -u
D="$1"; shift
h() { echo; echo "=== $* ==="; }
pids() { for s in skep-agent skep-api skep-cluster; do printf "%s=%s " $s "$(pgrep -x $s | tr '\n' ' ')"; done; echo; }
cd "$D/pkg" || exit 9
h "before"; date -u +%Y-%m-%dT%H:%M:%SZ; hostname
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
h "after"
/usr/local/sbin/skep-agent -version; /usr/local/sbin/skep-cluster version
pids; sha256 -q /etc/rc.conf; sha256 -q /boot/loader.conf
echo "vid=$(sysctl -n hw.vmm.vmx.cap.virtual_interrupt_delivery 2>&1)"
