#!/bin/sh
# Probe (c), third try: is the reader alive, does it keep reading, what does it hold.
P=$1/zvolprobe; v=$P/a7
[ -c /dev/zvol/$v ] || { zfs set volmode=none $v; zfs set volmode=dev $v; sleep 1; }
E=/var/tmp/zvolprobe-dd3.err
dd if=/dev/zvol/$v of=/dev/null bs=64k speed=65536 2>$E &
dd=$!
( sleep 120; kill $dd 2>/dev/null ) &
wd=$!
st() { echo "  [$1] ps: $(ps -o pid,stat,wchan -p $dd | tail -n +2)"; kill -INFO $dd 2>/dev/null; sleep 0.3; echo "  [$1] dd: $(tail -1 $E)"; echo "  [$1] fd: $(procstat -f $dd 2>/dev/null | awk '$3==3')"; }
sleep 3; st before
zfs set volmode=none $v; echo "set none rc=$? node=$([ -c /dev/zvol/$v ] && echo present || echo gone)"
sleep 3; st after-none
zfs set volmode=dev $v; echo "set dev rc=$? node=$([ -c /dev/zvol/$v ] && echo present || echo gone)"
sleep 3; st after-dev
kill $dd 2>/dev/null; wait $dd; echo "dd exit status $?"
kill $wd 2>/dev/null
echo "full dd stderr:"; cat $E; rm -f $E
