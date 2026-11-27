#!/bin/bash
# 8e-1 job 6: block TCP port 8444 between the three lab addresses on ONE VM
# (smith or star only), in the anchor skep8e1, with a detached 10-minute
# timer that disables pf and flushes the anchor and the main ruleset.
# Nothing is written to rc.conf, loader.conf or pf.conf; port 22, 8443 and
# 8445 are never matched.
# Usage: 8e1-pf.sh info|block|unblock HOST   (HOST 10.79.20.81 or 10.79.20.83)
set -u
on() { ssh -o BatchMode=yes -o ConnectTimeout=10 aba@$1 "$2"; }
x=${2:-}
case "$x" in 10.79.20.81|10.79.20.83) ;; *) echo "pf only on smith (.81) or star (.83)"; exit 2;; esac
others=$(for a in 10.79.20.81 10.79.20.82 10.79.20.83; do [ "$a" != "$x" ] && printf '%s, ' "$a"; done | sed 's/, $//')
rules="block drop quick proto tcp from $x to { $others } port 8444
block drop quick proto tcp from $x port 8444 to { $others }
block drop quick proto tcp from { $others } to $x port 8444
block drop quick proto tcp from { $others } port 8444 to $x"
case ${1:-} in
info)
	on $x 'kldstat | grep -w pf || echo "pf module not loaded"; sudo pfctl -s info 2>&1 | head -3; echo "main rules:"; sudo pfctl -s rules 2>&1; echo "anchor skep8e1:"; sudo pfctl -a skep8e1 -s rules 2>&1' ;;
block)
	echo "rules for $x:"; echo "$rules"
	on $x "kldstat -q -m pf || sudo kldload pf; [ -z \"\$(sudo pfctl -s rules 2>/dev/null)\" ] || { echo 'pf already has a ruleset: stop'; exit 3; }"
	[ $? = 0 ] || exit 3
	printf '%s\n' "$rules" | on $x 'cat > /tmp/skep8e1.rules && sudo pfctl -n -a skep8e1 -f /tmp/skep8e1.rules && echo "anchor \"skep8e1\"" | sudo pfctl -n -f - && echo syntax-ok' || exit 4
	# The timer first: it survives the ssh session (daemon -f detaches).
	on $x 'sudo daemon -f -p /var/run/skep8e1-pf.pid sh -c "sleep 600; pfctl -d; pfctl -a skep8e1 -F rules; pfctl -F rules"; sleep 1; echo "timer pid $(sudo cat /var/run/skep8e1-pf.pid)"'
	on $x 'echo "anchor \"skep8e1\"" | sudo pfctl -f - && sudo pfctl -a skep8e1 -f /tmp/skep8e1.rules && sudo pfctl -e; rm -f /tmp/skep8e1.rules; sudo pfctl -s info | head -1; sudo pfctl -s rules; sudo pfctl -a skep8e1 -s rules' ;;
unblock)
	on $x 'sudo pfctl -a skep8e1 -F rules; sudo pfctl -F rules; sudo pfctl -d; p=$(sudo cat /var/run/skep8e1-pf.pid 2>/dev/null) && sudo pkill -F /var/run/skep8e1-pf.pid; sudo rm -f /var/run/skep8e1-pf.pid; sudo pfctl -s info | head -1; echo "main:"; sudo pfctl -s rules; echo "anchor:"; sudo pfctl -a skep8e1 -s rules' ;;
*) sed -n 2,8p "$0"; exit 2 ;;
esac
