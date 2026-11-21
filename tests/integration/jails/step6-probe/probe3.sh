#!/bin/sh
set -u
D=/skeptest/step6-probe
J=skep-build-probe
A="allow.set_hostname=0 allow.sysvipc=0 allow.raw_sockets=0 allow.chflags=0 allow.mount=0 allow.quotas=0 allow.socket_af=0 allow.mlock=0 allow.reserved_ports=0 allow.read_msgbuf=0 allow.unprivileged_proc_debug=0 allow.unprivileged_parent_tampering=0 allow.suser=0 allow.nfsd=0 allow.extattr=0 allow.adjtime=0 allow.settime=0 allow.routing=0 allow.setaudit=0"
jail -c name=$J path=$D/root host.hostname=$J persist ip4=disable ip6=disable enforce_statfs=2 children.max=0 securelevel=2 exec.clean $A || exit 1
echo "--- env under jexec -l (no -U), uid"; jexec -l $J /usr/bin/env; jexec -l $J /usr/bin/id
echo "--- pwd_mkdb via jexec -l (no -U)"; jexec -l $J /usr/sbin/pwd_mkdb -i -p -d /etc /etc/master.passwd; echo "exit $?"
echo "--- env under jexec -l -U root, after pwd.db"; jexec -l -U root $J /usr/bin/env; echo "exit $?"
echo "--- certctl -l -U root"; jexec -l -U root $J /usr/sbin/certctl rehash; echo "exit $?"
echo "--- services_mkdb"; jexec -l -U root $J /usr/sbin/services_mkdb -l -q -o /var/db/services.db /etc/services; echo "exit $?"
echo "--- cap_mkdb"; jexec -l -U root $J /usr/bin/cap_mkdb -l /etc/login.conf; echo "exit $?"
echo "--- makewhatis"; jexec -l -U root $J /usr/bin/makewhatis /usr/share/man; echo "exit $?"
[ -d $D/root/usr/share/openssl/man ] && { jexec -l -U root $J /usr/bin/makewhatis /usr/share/openssl/man; echo "exit $? (openssl)"; }
jail -r $J; echo "jail -r exit $?"; jls -j $J >/dev/null 2>&1; echo "jls -j exit $? (1 = gone)"
echo "--- outputs vs trace-7b (sha256)"
for f in etc/pwd.db etc/spwd.db var/db/services.db etc/login.conf.db; do
  a=$(sha256 -q $D/root/$f 2>/dev/null); b=$(sha256 -q /skeptest/trace-7b/root/$f 2>/dev/null)
  [ "$a" = "$b" ] && r=same || r=DIFFERENT; echo "$f $r"; done
echo "certs: $(ls $D/root/etc/ssl/certs | wc -l) vs $(ls /skeptest/trace-7b/root/etc/ssl/certs | wc -l)"
diff -r $D/root/etc/ssl/certs /skeptest/trace-7b/root/etc/ssl/certs >/dev/null && echo "certs identical" || echo "certs differ"
ls -l $D/root/usr/share/man/mandoc.db $D/root/usr/share/openssl/man/mandoc.db 2>&1
