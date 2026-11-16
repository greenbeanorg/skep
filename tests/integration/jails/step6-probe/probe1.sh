#!/bin/sh
# Step 6 feasibility probe: scripts-off install into skeptest/step6-probe.
set -u
D=/skeptest/step6-probe
zfs create skeptest/step6-probe || exit 1
chmod 0700 $D
mkdir -p $D/root $D/work/tmp $D/work/cache
env -i PATH=/sbin:/bin:/usr/sbin:/usr/bin ASSUME_ALWAYS_YES=yes TMPDIR=$D/work/tmp \
  /usr/sbin/pkg --rootdir $D/root -o PKG_CACHEDIR=$D/work/cache -o PKG_DBDIR=$D/root/var/db/pkg \
  -o RUN_SCRIPTS=false -o PKG_TRIGGERS_ENABLE=false install -y FreeBSD-set-minimal > $D/work/install.log 2>&1
echo "pkg exit $?"
grep -ci 'trigger\|script' $D/work/install.log
ls -la $D/root/var/db/pkg/ ; ls -la $D/root/var/db/pkg/triggers 2>&1
ls -l $D/root/etc/pwd.db $D/root/etc/spwd.db $D/root/var/db/services.db $D/root/etc/login.conf.db 2>&1
ls $D/root/etc/ssl/certs 2>&1 | wc -l
ls $D/work/tmp | wc -l
grep USERLAND_VERSION= $D/root/bin/freebsd-version
