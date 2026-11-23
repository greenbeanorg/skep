#!/bin/sh
# zvol device-node probe (ADR 0010 amendment z). Run as root.
# Usage: probe.sh POOL PART   PART = setup | a | b | c | d | e | cleanup
# Only ever writes volmode=dev or volmode=none. Works only under POOL/zvolprobe.
set -u
POOL=$1
PART=$2
P=$POOL/zvolprobe
KEY=/var/tmp/zvolprobe.key
GPT=/var/tmp/zvolprobe-gpt64.img

node() { [ -c "/dev/zvol/$1" ]; }
# wait_node DS SECS: poll every 0.1 s; prints the elapsed tenths or "never"
wait_node() {
	_n=0
	_max=$(($2 * 10))
	while [ "$_n" -le "$_max" ]; do
		if node "$1"; then echo "$_n"; return 0; fi
		sleep 0.1
		_n=$((_n + 1))
	done
	echo never
	return 1
}
# toggle DS: none then dev; prints what happened
toggle() {
	_e1=$(zfs set volmode=none "$1" 2>&1); _r1=$?
	sleep 0.5
	if node "$1"; then _under=present; else _under=gone; fi
	_e2=$(zfs set volmode=dev "$1" 2>&1); _r2=$?
	_t=$(wait_node "$1" 10)
	echo "toggle $1 none_rc=$_r1 ${_e1:+none_err=[$_e1]} node_under_none=$_under dev_rc=$_r2 ${_e2:+dev_err=[$_e2]} node_after_tenths=$_t volmode=$(zfs get -H -o value volmode "$1")"
	[ "$_t" != never ]
}
geom() {
	_c=$(sysctl -n kern.geom.conftxt | grep -c zvol)
	echo "geom: lines with zvol in kern.geom.conftxt: $_c"
	sysctl -n kern.geom.conftxt | grep zvol
}

case $PART in
setup)
	if zfs list "$P" >/dev/null 2>&1; then zfs destroy -r "$P" || exit 1; fi
	zfs create -o mountpoint=none -o canmount=off "$P" || exit 1
	uname -a; zfs version; sysctl vfs.zfs.vol.mode
	echo "setup ok"
	;;
a)
	i=0 miss0=0 miss1=0 missing=""
	while [ "$i" -lt 50 ]; do
		zfs create -s -V 1G -o volblocksize=16K -o volmode=dev "$P/a$i" || exit 1
		if node "$P/a$i"; then n0=Y; else n0=N; miss0=$((miss0 + 1)); fi
		sleep 1
		if node "$P/a$i"; then n1=Y; else n1=N; miss1=$((miss1 + 1)); missing="$missing a$i"; fi
		echo "create a$i node_at_0s=$n0 node_at_1s=$n1"
		i=$((i + 1))
	done
	echo "RESULT a: missing at 0 s: $miss0/50; missing at 1 s: $miss1/50:$missing"
	s=0
	while [ "$s" -lt 30 ]; do
		sleep 1; s=$((s + 1))
		still=""
		for v in $missing; do
			if node "$P/$v"; then echo "late: $v appeared ${s} s after the loop"; else still="$still $v"; fi
		done
		missing=$still
	done
	c=0; for v in $missing; do c=$((c + 1)); done
	echo "RESULT a: still missing 30 s after the loop (>= 31 s after create): $c:$missing"
	geom
	;;
b)
	echo "--- b1 plain zvols missing from (a)"
	ok=0 bad=0 tried=0
	for v in $(zfs list -H -o name -t volume -d 1 "$P"); do
		node "$v" && continue
		tried=$((tried + 1))
		if toggle "$v"; then ok=$((ok + 1)); else bad=$((bad + 1)); fi
	done
	if [ "$tried" -eq 0 ]; then echo "no plain zvol was missing; toggling $P/a0 (present)"; toggle "$P/a0" && ok=1 || bad=1; tried=1; fi
	echo "RESULT b1 plain: toggled $tried, node back $ok, not back $bad"

	echo "--- b2 clones"
	zfs snapshot "$P/a0@c" || exit 1
	i=0 miss=0 ok=0 bad=0
	while [ "$i" -lt 20 ]; do
		zfs clone -o volmode=dev "$P/a0@c" "$P/c$i" || exit 1
		sleep 1
		if node "$P/c$i"; then echo "clone c$i node=Y"; else
			miss=$((miss + 1)); echo "clone c$i node=N"
			if toggle "$P/c$i"; then ok=$((ok + 1)); else bad=$((bad + 1)); fi
		fi
		i=$((i + 1))
	done
	if [ "$miss" -eq 0 ]; then echo "no clone was missing; toggling $P/c0 (present)"; toggle "$P/c0" && ok=1 || bad=1; fi
	echo "RESULT b2 clone: missing $miss/20, toggled node back $ok, not back $bad"

	echo "--- b3 rollbacks (1 MiB written between snapshot and rollback)"
	i=1 miss=0 ok=0 bad=0
	while [ "$i" -le 20 ]; do
		v="$P/a$i"
		node "$v" || toggle "$v" >/dev/null
		zfs snapshot "$v@r" || exit 1
		timeout 30 dd if=/dev/random of="/dev/zvol/$v" bs=1m count=1 2>/dev/null || echo "dd write a$i failed rc=$?"
		zfs rollback "$v@r" || exit 1
		sleep 1
		if node "$v"; then echo "rollback a$i node=Y"; else
			miss=$((miss + 1)); echo "rollback a$i node=N"
			if toggle "$v"; then ok=$((ok + 1)); else bad=$((bad + 1)); fi
		fi
		i=$((i + 1))
	done
	if [ "$miss" -eq 0 ]; then echo "no rolled-back zvol was missing; toggling $P/a1 (present)"; toggle "$P/a1" && ok=1 || bad=1; fi
	echo "RESULT b3 rollback: missing $miss/20, toggled node back $ok, not back $bad"

	echo "--- b4 encrypted dataset, key loaded"
	umask 077
	dd if=/dev/random of="$KEY" bs=32 count=1 2>/dev/null
	zfs create -o encryption=aes-256-gcm -o keyformat=raw -o "keylocation=file://$KEY" -o mountpoint=none -o canmount=off "$P/enc" || exit 1
	echo "keystatus: $(zfs get -H -o value keystatus "$P/enc")"
	i=0 miss=0 ok=0 bad=0
	while [ "$i" -lt 20 ]; do
		zfs create -s -V 1G -o volblocksize=16K -o volmode=dev "$P/enc/e$i" || exit 1
		sleep 1
		if node "$P/enc/e$i"; then echo "enc e$i node=Y"; else
			miss=$((miss + 1)); echo "enc e$i node=N"
			if toggle "$P/enc/e$i"; then ok=$((ok + 1)); else bad=$((bad + 1)); fi
		fi
		i=$((i + 1))
	done
	if [ "$miss" -eq 0 ]; then echo "no encrypted zvol was missing; toggling $P/enc/e0 (present)"; toggle "$P/enc/e0" && ok=1 || bad=1; fi
	echo "RESULT b4 encrypted: missing $miss/20, toggled node back $ok, not back $bad"
	geom
	;;
c)
	v="$P/a5"
	node "$v" || toggle "$v"
	timeout 300 dd if="/dev/zvol/$v" of=/dev/null bs=64k speed=65536 2>/var/tmp/zvolprobe-dd.err &
	ddpid=$!
	sleep 2
	echo "reader pid $ddpid alive: $(kill -0 $ddpid 2>/dev/null && echo yes || echo no)"
	fstat "/dev/zvol/$v" 2>&1 | head -3
	e1=$(zfs set volmode=none "$v" 2>&1); r1=$?
	sleep 1
	echo "set none with node open: rc=$r1 err=[$e1] volmode=$(zfs get -H -o value volmode "$v") node=$(node "$v" && echo present || echo gone) reader_alive=$(kill -0 $ddpid 2>/dev/null && echo yes || echo no)"
	e2=$(zfs set volmode=dev "$v" 2>&1); r2=$?
	t=$(wait_node "$v" 10)
	echo "set dev with reader still running: rc=$r2 err=[$e2] node_after_tenths=$t volmode=$(zfs get -H -o value volmode "$v") reader_alive=$(kill -0 $ddpid 2>/dev/null && echo yes || echo no)"
	sleep 2
	echo "2 s later: node=$(node "$v" && echo present || echo gone) reader_alive=$(kill -0 $ddpid 2>/dev/null && echo yes || echo no)"
	kill $ddpid 2>/dev/null; wait $ddpid; echo "reader exit status $?"
	echo "dd stderr:"; cat /var/tmp/zvolprobe-dd.err; rm -f /var/tmp/zvolprobe-dd.err
	echo "after reader ended: node=$(node "$v" && echo present || echo gone)"
	node "$v" || { echo "repairing after the open test"; toggle "$v"; }
	geom
	;;
d)
	[ -f "$GPT" ] || { echo "no $GPT"; exit 1; }
	v="$P/g0"
	zfs create -V 64M -o volblocksize=16K -o volmode=dev "$v" || exit 1
	sleep 1
	node "$v" || { echo "g0 node missing after create; repairing first"; toggle "$v"; }
	timeout 60 dd if="$GPT" of="/dev/zvol/$v" bs=1m 2>&1 | tail -1
	echo "sha256 image $(sha256 -q "$GPT") zvol $(timeout 60 sha256 -q "/dev/zvol/$v")"
	echo "before the repair:"; ls -l /dev/zvol/$P/ | grep g0; geom
	toggle "$v"
	echo "after the repair:"; ls -l /dev/zvol/$P/ | grep g0
	if [ -e "/dev/zvol/$v""p1" ]; then echo "RESULT d: p1 EXISTS"; else echo "RESULT d: no /dev/zvol/${v}p1"; fi
	geom
	echo "kern.geom.conftxt lines naming g0: $(sysctl -n kern.geom.conftxt | grep -c g0)"
	echo "gpart on it (expect nothing):"; gpart show "zvol/$v" 2>&1 | head -2
	;;
e)
	# 50 zvols (a0..a49) only; others removed first.
	for c in $(zfs list -H -o name -t volume -d 1 "$P" | grep '/c[0-9]*$'); do zfs destroy "$c"; done
	zfs list "$P/enc" >/dev/null 2>&1 && zfs destroy -r "$P/enc"
	zfs list "$P/g0" >/dev/null 2>&1 && zfs destroy -r "$P/g0"
	for s in $(zfs list -H -o name -t snapshot -r "$P"); do zfs destroy "$s"; done
	rm -f "$KEY"
	echo "volumes: $(zfs list -H -o name -t volume -r "$P" | wc -l)"
	rep=1 last=0
	while [ "$rep" -le 3 ]; do
		fix=0
		for v in $(zfs list -H -o name -t volume -r "$P"); do node "$v" || { toggle "$v" >/dev/null && fix=$((fix + 1)); }; done
		have=0; for v in $(zfs list -H -o name -t volume -r "$P"); do node "$v" && have=$((have + 1)); done
		echo "repeat $rep: before export $have/50 nodes ($fix repaired first)"
		zpool export "$POOL" || { echo "export failed"; exit 1; }
		echo "after export: $(ls /dev/zvol/$P 2>/dev/null | wc -l) nodes under /dev/zvol/$P"
		zpool import "$POOL" || { echo "import failed"; exit 1; }
		for w in 0 10 30; do
			[ "$w" -gt 0 ] && sleep $((w - last))
			last=$w
			have=0 miss=""
			for v in $(zfs list -H -o name -t volume -r "$P"); do if node "$v"; then have=$((have + 1)); else miss="$miss ${v##*/}"; fi; done
			echo "RESULT e repeat $rep: ${w} s after import: $have/50 nodes; missing:$miss"
		done
		last=0
		rep=$((rep + 1))
	done
	geom
	;;
cleanup)
	zfs list "$P" >/dev/null 2>&1 && zfs destroy -r "$P"
	rm -f "$KEY" "$GPT" /var/tmp/zvolprobe-dd.err /var/tmp/probe.sh
	zfs list -r "$POOL"; geom
	;;
esac
