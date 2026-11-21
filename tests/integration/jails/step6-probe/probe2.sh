#!/bin/sh
set -u
D=/skeptest/step6-probe
J=skep-build-probe
A="allow.set_hostname=0 allow.sysvipc=0 allow.raw_sockets=0 allow.chflags=0 allow.mount=0 allow.quotas=0 allow.socket_af=0 allow.mlock=0 allow.reserved_ports=0 allow.read_msgbuf=0 allow.unprivileged_proc_debug=0 allow.unprivileged_parent_tampering=0 allow.suser=0 allow.nfsd=0 allow.extattr=0 allow.adjtime=0 allow.settime=0 allow.routing=0 allow.setaudit=0"
jail -c name=$J path=$D/root host.hostname=$J persist ip4=disable ip6=disable enforce_statfs=2 children.max=0 securelevel=2 exec.clean $A
echo "jail -c exit $?"
jls --libxo=json -n -j $J > $D/work/jls.json; echo "jls exit $?"
echo "--- jexec -l -U root certctl (before pwd.db)"
jexec -l -U root $J /usr/sbin/certctl rehash; echo "exit $?"
echo "--- jexec -U root (no -l)"
jexec -U root $J /usr/sbin/certctl rehash; echo "exit $?"
echo "--- jexec -l (no -U)"
jexec -l $J /usr/sbin/certctl rehash; echo "exit $?"
echo "--- jexec plain"
jexec $J /usr/sbin/certctl rehash; echo "exit $?"
jail -r $J; echo "jail -r exit $?"
jls -j $J; echo "jls -j exit $? (expect nonzero)"
