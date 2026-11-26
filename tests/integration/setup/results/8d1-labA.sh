#!/bin/bash
# 8d-1 lab, part A: blazanov (10.79.20.84, VM 944 on garrett) as a single
# host, driven from merrick over ssh as aba (sudo on the VM). From
# 8c2-labA.sh. Passwords are read from 0600 files under ~/skep-lab-creds
# and never printed (8d1-api.py sends them only in a login body).
#
# Usage: 8d1-labA.sh deploy DIST            copy DIST/pkg and the agentsock helper to ~/skep-8d1 on blazanov
#        8d1-labA.sh install                pkg create + pkg install -y -f, then skep-setup upgrade -yes -keep 50
#        8d1-labA.sh state                  services, PIDs, cluster, vid, root-owned files
#        8d1-labA.sh sock root|skep ARGS... the agentsock helper as root or as the skep account
#        8d1-labA.sh vm status|shutdown|rollback|start [SNAP]   VM 944 only
#        8d1-labA.sh run CMD...             one command on blazanov
set -u
B=10.79.20.84
G=10.79.10.25
W=skep-8d1
on() { ssh -o BatchMode=yes -o ConnectTimeout=10 aba@$B "$@"; }
h() { echo; echo "=== $(date -u +%H:%M:%SZ) $* ==="; }
waitssh() { local i=0; while [ $i -lt 300 ]; do on true 2>/dev/null && { echo "ssh up after ${i}s"; return 0; }; sleep 5; i=$((i+5)); done; echo "TIMEOUT: no ssh"; return 1; }

case ${1:-} in
deploy)
	dist=$2
	h "deploy $dist/pkg and agentsock to blazanov ~/$W"
	on "rm -rf $W && mkdir -p $W"
	tar -C "$dist" -cf - pkg | on "tar -C $W -xf -"
	scp -q bin/freebsd-amd64-lab/agentsock aba@$B:$W/agentsock
	on "chmod 755 $W/agentsock; sha256 -r $W/pkg/stage/usr/local/sbin/* | sed 's,.*/,,'" ;;
install)
	h "pkg create + pkg install -y -f + skep-setup upgrade"
	on "cd $W/pkg && rm -rf out && mkdir out && pkg create -M +MANIFEST -p plist -r stage -o out && sudo pkg install -y -f ./out/skep-0.1.0.pkg >/dev/null; echo pkg-exit=\$?"
	on "sudo skep-setup upgrade -yes -keep 50; echo upgrade-exit=\$?"
	on "/usr/local/sbin/skep-agent -version" ;;
state)
	h "blazanov state"
	on 'for s in skep-agent skep-cluster skep-api; do printf "%s=%s " $s "$(pgrep -x $s | tr "\n" " ")"; done; echo
		/usr/local/sbin/skep-agent -version
		sudo skep-cluster status 2>&1 | egrep "Cluster ID|Raft|State machine|Build|member|Health"
		echo "vid=$(sysctl -n hw.vmm.vmx.cap.virtual_interrupt_delivery)"
		echo "root-owned in /var/db/skep/cluster: [$(sudo find /var/db/skep/cluster -user root 2>/dev/null | tr "\n" " ")]"' ;;
sock)
	shift; who=$1; shift
	if [ "$who" = root ]; then on "sudo ./$W/agentsock $*"; else on "sudo -u skep ./$W/agentsock $*"; fi ;;
vm)
	act=$2; snap=${3:-}
	h "garrett: VM 944 $act $snap"
	case $act in
	status) ssh -o BatchMode=yes root@$G "qm status 944" ;;
	shutdown) ssh -o BatchMode=yes root@$G "qm shutdown 944; qm status 944" ;;
	rollback) ssh -o BatchMode=yes root@$G "qm rollback 944 $snap; qm status 944" ;;
	start) ssh -o BatchMode=yes root@$G "qm start 944; qm status 944" && waitssh ;;
	esac ;;
run) shift; on "$@"; echo "exit=$?" ;;
*) sed -n '2,15p' "$0"; exit 2 ;;
esac
