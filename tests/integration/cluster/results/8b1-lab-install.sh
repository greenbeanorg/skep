#!/bin/sh
# 8b-1 lab, job 4: a first install on star by the installation guide
# (docs/installation/packaging.md sections 3 and 4, docs/installation/skep-setup.md),
# with langrishe's answers. "skep-setup apply" (with -admin) is run separately
# over a pty, so its password prompt can be answered. Run as aba (sudo).
set -u
D="$1"
A="-mgmt vtnet0 -pool zroot -no-racct"
h() { echo; echo "=== $* ==="; }
cd "$D/pkg" || exit 9
h "before"; date -u +%Y-%m-%dT%H:%M:%SZ; hostname; freebsd-version -ku
sha256 -q /etc/rc.conf; sha256 -q /boot/loader.conf
grep -n virtual_interrupt_delivery /boot/loader.conf || { echo "NO VID TUNABLE LINE: stop"; exit 8; }
echo "vid=$(sysctl -n hw.vmm.vmx.cap.virtual_interrupt_delivery 2>&1)"; kldstat | grep -c vmm
ifconfig vtnet1 | egrep 'inet|status'
h "3. create the package"
sha256 -r stage/usr/local/sbin/* stage/usr/local/bin/* | sed 's,stage/,,'
rm -rf out; mkdir -p out
pkg create -M +MANIFEST -p plist -r stage -o out || exit 1
pkg info -F out/skep-0.1.0.pkg | egrep 'Name|Version|Architecture'
pkg info -l -F out/skep-0.1.0.pkg
h "4.1 pkg install ./out/skep-0.1.0.pkg"
echo y | sudo pkg install ./out/skep-0.1.0.pkg; echo "exit=$?"
id skep; id skep-cluster; sha256 -q /etc/rc.conf
h "skep-setup check $A"
sudo skep-setup check $A; echo "exit=$?"
h "skep-setup plan $A -start -admin admin"
sudo skep-setup plan $A -start -admin admin; echo "exit=$?"
