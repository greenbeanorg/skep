#!/bin/sh
# ADR 0010 step 7 probe: start a jail on a clone of the step 6 template with
# Decision 9's baseline, as jail(8) would get it from Skep's generated
# jail.conf. Usage: sudo sh -s VARIANT < probe-jail.sh
#   A: every allow.* off except allow.suser (the strict reading)
#   B: as A, plus allow.reserved_ports=1 (its default)
# Writes /skeptest/step7-probe/jail-VARIANT.conf and console-VARIANT.log.
set -eu
V=$1
D=/skeptest/step7-probe
NAME=skep-probe7
extra=""
[ "$V" = B ] && extra="allow.reserved_ports = 1;"
off=""
for a in adjtime chflags extattr mlock mount nfsd quotas raw_sockets read_msgbuf reserved_ports routing \
	set_hostname setaudit settime socket_af sysvipc unprivileged_parent_tampering unprivileged_proc_debug; do
	off="$off allow.$a = 0;"
done
cat > $D/jail-$V.conf <<CONF
$NAME {
	path = "$D/root";
	host.hostname = "probe7";
	vnet;
	persist;
	enforce_statfs = 2;
	children.max = 0;
	securelevel = 2;
	mount.devfs;
	devfs_ruleset = 5;
	$off
	allow.suser = 1;
	$extra
	exec.clean;
	exec.start = "/bin/sh /etc/rc";
	exec.stop = "/bin/sh /etc/rc.shutdown jail";
	exec.consolelog = "$D/console-$V.log";
	exec.timeout = 60;
	stop.timeout = 10;
}
CONF
chmod 0600 $D/jail-$V.conf
: > $D/console-$V.log
echo "== jail -c ($V)"
jail -f $D/jail-$V.conf -c $NAME; echo "jail -c exit $?"
echo "== console.log"; cat $D/console-$V.log
echo "== jls"; jls -j $NAME jid name path vnet securelevel enforce_statfs children.max devfs_ruleset
echo "== allow.* as the kernel reports them"
jls -n -j $NAME | tr ' ' '\n' | grep '^allow\.' | grep -v '^allow\.no' | tr '\n' ' '; echo
echo "== processes in the jail"; ps -J $NAME -o pid,user,command 2>/dev/null || jexec $NAME ps -ax
echo "== devices in the jail"; ls /skeptest/step7-probe/root/dev | tr '\n' ' '; echo
