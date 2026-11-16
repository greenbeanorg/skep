#!/bin/sh
set -u
D=/skeptest/step6-probe; R=$D/root; T=/skeptest/trace-7b/root
J=skep-build-probec
jail -c name=$J path=$R host.hostname=x persist ip4=disable ip6=disable enforce_statfs=2 children.max=0 securelevel=2 exec.clean allow.suser=0 allow.set_hostname=0 allow.mount=0 allow.chflags=0 allow.raw_sockets=0 allow.sysvipc=0 || exit 1
mkdir -p $R/tmp/s
jexec -l $J /usr/sbin/services_mkdb -l -q -o /tmp/s/jail1.db /etc/services; echo "[$?] jail1"
jexec -l $J /usr/sbin/services_mkdb -l -q -o /tmp/s/jail2.db /etc/services; echo "[$?] jail2"
jexec -l $J /usr/sbin/services_mkdb -l -q -o /tmp/s/jaildir.db /; echo "[$?] jail, input / (package form)"
jail -r $J; jls -j $J >/dev/null 2>&1; echo "removed: jls exit $?"
# The package's own form, host binary, as the trace ran it (writes only into the probe dataset):
/usr/sbin/services_mkdb -l -q -o $D/work/hostdir.db $R/; echo "[$?] host, input <root>/"
/usr/sbin/services_mkdb -l -q -o $D/work/hostdir2.db $R/; echo "[$?] host, input <root>/ again"
cmp -s $R/etc/services $T/etc/services && echo "etc/services identical to trace" || echo "etc/services DIFFERS from trace"
for f in $R/tmp/s/jail1.db $R/tmp/s/jail2.db $R/tmp/s/jaildir.db $D/work/hostdir.db $D/work/hostdir2.db $R/var/db/services.db $T/var/db/services.db; do
  echo "$(sha256 -q $f) $(stat -f %z $f) $f"; done
echo "first differing bytes trace vs jail1:"; cmp -l $T/var/db/services.db $R/tmp/s/jail1.db | wc -l
cmp -l $T/var/db/services.db $R/tmp/s/jail1.db | head -5
rm -rf $R/tmp/s
