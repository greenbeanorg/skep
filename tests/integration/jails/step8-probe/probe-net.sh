#!/bin/sh
# ADR 0010 step 8 probe: Decision 10 by hand, before any code. A VNET jail
# on a clone of minimal-ssh-151 with one epair: the host end described
# skep:probe8:0 and added to bridge0, the jail end renamed on the host and
# moved in by jail(8)'s vnet.interface. Addressing by DHCP from the jail's
# rc.conf; sshd with a throwaway key.
#
# Usage (as root on hearst; if_epair must already be loaded):
#   sh probe-net.sh up 'ssh-ed25519 AAAA... comment'
#   sh probe-net.sh down
# Everything lives under skeptest/step8-probe; nothing else is touched
# except bridge0's member list (the epair's host end).
set -eu
P=skeptest/step8-probe
D=/skeptest/step8-probe
TPL=skeptest/skep/templates/dbf4243a-81fe-4914-8f7a-fd0a768476aa  # minimal-ssh-151
NAME=skep-probe8
JIF=sjprobe8n0          # the jail end's stable name (<= 15 characters)
JMAC=02:5b:8e:00:00:01  # locally administered, fixed
DESC=skep:probe8:0

case "$1" in
up)
	KEY=$2
	kldstat -q -m if_epair || { echo "if_epair is not loaded"; exit 1; }
	zfs create -o mountpoint=$D $P
	chown root:wheel $D; chmod 0700 $D
	zfs clone -o readonly=off $TPL@skep-base $P/root
	echo "== the jail's rc.conf and key"
	cat > $D/root/etc/rc.conf <<RC
hostname="probe8"
ifconfig_$JIF="SYNCDHCP"
sshd_enable="YES"
sshd_flags="-o PermitRootLogin=prohibit-password"
RC
	cat $D/root/etc/rc.conf
	install -d -m 0700 -o root -g wheel $D/root/root/.ssh
	printf '%s\n' "$KEY" > $D/root/root/.ssh/authorized_keys
	chmod 0600 $D/root/root/.ssh/authorized_keys

	echo "== epair create"
	A=$(ifconfig epair create)
	B=${A%a}b
	echo "created $A (jail end $B)"
	echo "$A" > $D/epair
	ifconfig "$A" description "$DESC" up
	ifconfig "$B" name "$JIF"
	ifconfig "$JIF" ether "$JMAC"
	ifconfig bridge0 addm "$A"
	echo "== host end after addm"; ifconfig "$A"
	echo "== jail end before the move"; ifconfig "$JIF"

	off=""
	for a in adjtime chflags extattr mlock mount nfsd quotas raw_sockets read_msgbuf routing \
		set_hostname setaudit settime socket_af sysvipc unprivileged_parent_tampering unprivileged_proc_debug; do
		off="$off allow.$a = 0;"
	done
	cat > $D/jail.conf <<CONF
$NAME {
	path = "$D/root";
	host.hostname = "probe8";
	vnet;
	vnet.interface = "$JIF";
	persist;
	enforce_statfs = 2;
	children.max = 0;
	securelevel = 2;
	mount.devfs;
	devfs_ruleset = 5;
	$off
	allow.suser = 1;
	allow.reserved_ports = 1;
	exec.clean;
	exec.start = "/bin/sh /etc/rc";
	exec.stop = "/bin/sh /etc/rc.shutdown jail";
	exec.consolelog = "$D/console.log";
	exec.timeout = 120;
	stop.timeout = 10;
}
CONF
	chmod 0600 $D/jail.conf
	: > $D/console.log
	echo "== jail -c"
	jail -f $D/jail.conf -c $NAME; echo "jail -c exit $?"
	echo "== console.log"; cat $D/console.log
	echo "== in the jail: ifconfig, routes, resolv.conf"
	jexec $NAME ifconfig -a
	jexec $NAME netstat -rn
	jexec $NAME cat /etc/resolv.conf
	echo "== on the host: is $JIF still visible?"; ifconfig "$JIF" 2>&1 || true
	echo "== host end"; ifconfig "$A"
	echo "== bridge0 members"; ifconfig bridge0 | grep member
	;;
down)
	A=$(cat $D/epair)
	echo "== jail -r"
	jail -f $D/jail.conf -r $NAME; echo "jail -r exit $?"
	echo "== after jail -r: the jail end back on the host?"; ifconfig "$JIF" 2>&1 || true
	echo "== host end"; ifconfig "$A" 2>&1 || true
	echo "== bridge0 members"; ifconfig bridge0 | grep member || true
	echo "== destroy $A"
	ifconfig "$A" destroy; echo "exit $?"
	echo "== epairs left"; ifconfig -g epair || true
	ifconfig "$JIF" 2>&1 || true
	echo "== bridge0 members"; ifconfig bridge0 | grep member || true
	mount | grep step8-probe || true
	zfs destroy -r $P
	echo "== datasets left"; zfs list -r skeptest/step8-probe 2>&1 || true
	;;
*) echo "usage: $0 up KEY | down"; exit 2 ;;
esac
