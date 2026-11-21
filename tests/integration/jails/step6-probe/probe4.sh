#!/bin/sh
set -u
D=/skeptest/step6-probe
R=$D/root
B="host.hostname=x persist ip4=disable ip6=disable enforce_statfs=2 children.max=0 securelevel=2 exec.clean allow.set_hostname=0 allow.sysvipc=0 allow.raw_sockets=0 allow.chflags=0 allow.mount=0 allow.quotas=0 allow.socket_af=0 allow.mlock=0 allow.reserved_ports=0 allow.read_msgbuf=0 allow.unprivileged_proc_debug=0 allow.unprivileged_parent_tampering=0 allow.nfsd=0 allow.extattr=0 allow.adjtime=0 allow.settime=0 allow.routing=0 allow.setaudit=0"
run() { # jailname, jexec-flags...
  J=$1; shift
  for c in "/usr/sbin/certctl rehash" "/usr/sbin/pwd_mkdb -i -p -d /etc /etc/master.passwd" \
           "/usr/sbin/services_mkdb -l -q -o /var/db/services.db /etc/services" \
           "/usr/bin/cap_mkdb -l /etc/login.conf" "/usr/bin/makewhatis /usr/share/man" "/usr/bin/makewhatis /usr/share/openssl/man"; do
    jexec "$@" $J $c 2>&1 | head -2; echo "  [$?] $c"; done
}
cmp_out() {
  for f in etc/pwd.db etc/spwd.db var/db/services.db etc/login.conf.db; do
    a=$(sha256 -q $R/$f 2>/dev/null); b=$(sha256 -q /skeptest/trace-7b/root/$f 2>/dev/null)
    [ -n "$a" ] && [ "$a" = "$b" ] && r=same || r="DIFFERENT ($a)"; echo "  $f $r"; done
  diff -r $R/etc/ssl/certs /skeptest/trace-7b/root/etc/ssl/certs >/dev/null && echo "  certs identical" || echo "  certs differ"
  ls -l $R/usr/share/man/mandoc.db $R/usr/share/openssl/man/mandoc.db 2>&1 | sed 's/^/  /'
}
rm -f $R/etc/pwd.db $R/etc/spwd.db $R/var/db/services.db $R/etc/login.conf.db $R/usr/share/man/mandoc.db $R/usr/share/openssl/man/mandoc.db
echo "== (a) allow.suser=0, jexec -l (no -U), no devfs"
jail -c name=skep-build-probea path=$R $B allow.suser=0 || exit 1
run skep-build-probea -l
jail -r skep-build-probea; jls -j skep-build-probea >/dev/null 2>&1; echo "  removed: jls exit $?"
cmp_out
rm -f $R/etc/pwd.db $R/etc/spwd.db $R/var/db/services.db $R/etc/login.conf.db $R/usr/share/man/mandoc.db $R/usr/share/openssl/man/mandoc.db
echo "== (b) allow.suser=1, pwd_mkdb by jexec -l, then jexec -l -U root"
jail -c name=skep-build-probeb path=$R $B allow.suser=1 || exit 1
jexec -l skep-build-probeb /usr/sbin/pwd_mkdb -i -p -d /etc /etc/master.passwd; echo "  [$?] pwd_mkdb (-l only)"
run skep-build-probeb -l -U root
jail -r skep-build-probeb; jls -j skep-build-probeb >/dev/null 2>&1; echo "  removed: jls exit $?"
cmp_out
echo "== services.db sizes"; ls -l $R/var/db/services.db /skeptest/trace-7b/root/var/db/services.db
jls | wc -l
