#!/bin/bash
# 8c-2 lab, part A: blazanov (10.79.20.84, VM 944 on garrett) as a single
# host, driven from merrick over ssh as aba (sudo on the VM). Records go to
# ~/skep-test-logs; this script and its summary are the committed record.
# Passwords are read from 0600 files under ~/skep-lab-creds and never
# printed (8c2-ptytype.py types them into skep-api's prompt; 8c2-api.py
# sends them only in a login body).
#
# Usage: 8c2-labA.sh deploy NAME DIST      copy DIST/pkg to ~/skep-8c2/NAME on blazanov
#        8c2-labA.sh install NAME          pkg create + pkg install -y -f there
#        8c2-labA.sh state                 services, PIDs, rc.conf lines, journals, cluster, import state
#        8c2-labA.sh pids                  the three services' PIDs
#        8c2-labA.sh digest                user IDs and SHA-256 of each stored hash (never the hash)
#        8c2-labA.sh vm snapshot|rollback|start|shutdown NAME
#        8c2-labA.sh run CMD...            one command on blazanov (sudo is up to the caller)
set -u
B=10.79.20.84
G=10.79.10.25
on() { ssh -o BatchMode=yes -o ConnectTimeout=10 aba@$B "$@"; }
h() { echo; echo "=== $(date -u +%H:%M:%SZ) $* ==="; }
waitssh() { local i=0; while [ $i -lt 300 ]; do on true 2>/dev/null && { echo "ssh up after ${i}s"; return 0; }; sleep 5; i=$((i+5)); done; echo "TIMEOUT: no ssh"; return 1; }

case ${1:-} in
deploy)
	name=$2; dist=$3
	h "deploy $dist/pkg to blazanov ~/skep-8c2/$name"
	on "rm -rf skep-8c2/$name && mkdir -p skep-8c2/$name"
	tar -C "$dist" -cf - pkg | on "tar -C skep-8c2/$name -xf -"
	scp -q /tmp/claude-1000/-home-aba-git-skep/625de1a3-2c6f-4a2e-81cc-bfba3c83cf8e/scratchpad/hashdigest aba@$B:skep-8c2/hashdigest
	on "sha256 -r skep-8c2/$name/pkg/stage/usr/local/sbin/* | sed 's,.*/,,'" ;;
install)
	name=$2
	h "pkg create + pkg install -y -f ($name)"
	on "cd skep-8c2/$name/pkg && rm -rf out && mkdir out && pkg create -M +MANIFEST -p plist -r stage -o out && pkg info -F out/skep-0.1.0.pkg | egrep 'Name|Version' && sudo pkg install -y -f ./out/skep-0.1.0.pkg; echo pkg-exit=\$?"
	on "id skep; id skep-cluster 2>&1; /usr/local/sbin/skep-agent -version; ls /usr/local/sbin/skep-cluster 2>&1" ;;
pids)
	on 'for s in skep-agent skep-cluster skep-api; do printf "%s=%s " $s "$(pgrep -x $s | tr "\n" " ")"; done; echo' ;;
state)
	h "blazanov state"
	on 'for s in skep_agent skep_cluster skep_api; do sudo service $s status 2>&1 | head -1; done
		for s in skep-agent skep-cluster skep-api; do printf "%s=%s " $s "$(pgrep -x $s | tr "\n" " ")"; done; echo
		grep -E "^skep_" /etc/rc.conf; echo "rc.conf $(sha256 -q /etc/rc.conf) loader.conf $(sha256 -q /boot/loader.conf)"
		sudo ls /var/db/skep/setup 2>/dev/null | tr "\n" " "; echo
		ls -l /usr/local/etc/skep/ 2>&1
		if [ -x /usr/local/sbin/skep-cluster ]; then sudo skep-cluster status 2>&1 | egrep "Cluster ID|Raft|State machine|Listener|Build|Members|member|Health"; fi
		sudo skep-api admin list --import-state 2>&1 | sed "s/^/import state: /"
		sockstat -46l | egrep ":8444|:8445" || echo "nothing listens on 8444 or 8445"
		echo "root-owned in /var/db/skep/cluster: [$(sudo find /var/db/skep/cluster -user root 2>/dev/null | tr "\n" " ")]"' ;;
digest)
	on "sudo ./skep-8c2/hashdigest /var/db/skep/api/api.db" ;;
vm)
	act=$2; snap=${3:-}
	h "garrett: VM 944 $act $snap"
	case $act in
	snapshot) ssh -o BatchMode=yes root@$G "qm shutdown 944 --timeout 180 && qm snapshot 944 $snap && qm start 944 && qm listsnapshot 944" && waitssh ;;
	rollback) ssh -o BatchMode=yes root@$G "qm rollback 944 $snap && qm start 944; qm status 944" && waitssh ;;
	shutdown) ssh -o BatchMode=yes root@$G "qm shutdown 944 --timeout 180; qm status 944" ;;
	start) ssh -o BatchMode=yes root@$G "qm start 944; qm status 944" && waitssh ;;
	esac ;;
run) shift; on "$@"; echo "exit=$?" ;;
*) sed -n '2,20p' "$0"; exit 2 ;;
esac
