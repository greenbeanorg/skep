#!/bin/sh
# Part 3 on smith: test bridge9 on vtnet1 plus a hand-made epair; rc.conf
# variants in a copied root (the real /etc/rc.conf is not edited).
set -u
R=/tmp/skep-p3root
echo "before: vtnet1 flags: $(ifconfig vtnet1 | head -1)"
echo "before: ifconfig -l: $(ifconfig -l)"
echo "before: rc.conf sha256 $(sha256 -q /etc/rc.conf)"
EP=""
cleanup() {
  [ -n "$EP" ] && ifconfig "$EP" destroy && echo "destroyed $EP"
  ifconfig bridge9 >/dev/null 2>&1 && ifconfig bridge9 destroy && echo "destroyed bridge9"
  ifconfig vtnet1 down
  rm -rf "$R"
  echo "after: vtnet1 flags: $(ifconfig vtnet1 | head -1)"
  echo "after: ifconfig -l: $(ifconfig -l)"
  echo "after: rc.conf sha256 $(sha256 -q /etc/rc.conf)"
  echo "after: ifconfig -g bridge: [$(ifconfig -g bridge)]"
}
trap cleanup EXIT
ifconfig bridge9 create || exit 1
ifconfig bridge9 addm vtnet1 up || exit 1
EP=$(ifconfig epair create) || exit 1
ifconfig "$EP" up
ifconfig bridge9 addm "$EP" || exit 1
echo "created bridge9 (members vtnet1 $EP), vlanfilter off:"
ifconfig bridge9 | grep -E "bridge flags|member:"
echo "ifconfig -g bridge: [$(ifconfig -g bridge)]"

mkdir -m 700 "$R"; mkdir -p "$R/boot" "$R/var/run" "$R/var/db/skep" "$R/usr/local/share"
cp -Rp /etc "$R/etc"; cp -p /boot/loader.conf "$R/boot/"; cp -Rp /usr/local/etc "$R/usr/local/etc"
cp -p /var/run/dmesg.boot "$R/var/run/"; [ -d /var/db/skep/setup ] && cp -Rp /var/db/skep/setup "$R/var/db/skep/"
[ -d /usr/local/share/edk2-bhyve ] && cp -Rp /usr/local/share/edk2-bhyve "$R/usr/local/share/"
base=$(sha256 -q "$R/etc/rc.conf")

show() { # label
  echo; echo "=== $1"
  /usr/local/sbin/skep-setup plan -root "$2" -mgmt vtnet0 -bridge bridge9 2>&1 | awk '/bridge9/{p=1} p&&/^  (ok|warn|manual|add|refuse) /&&!/bridge9/{p=0} p'
}
show "(b)+(c) run-time bridge, real / (rc.conf has no bridge9, no cloned_interfaces)" /
printf 'cloned_interfaces="bridge9"\n' >> "$R/etc/rc.conf"
show "(b) run-time bridge already in cloned_interfaces (copied root)" "$R"
printf 'ifconfig_bridge9="addm vtnet1 up"\n' >> "$R/etc/rc.conf"
show "(c) bridge in rc.conf with addm vtnet1 only; $EP is a live member (copied root)" "$R"
printf 'ifconfig_bridge9="addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"\n' >> "$R/etc/rc.conf"
show "(a) vlanfilter in rc.conf, off now (copied root)" "$R"
echo; echo "--- copied rc.conf additions:"; diff /etc/rc.conf "$R/etc/rc.conf"
