#!/bin/sh
# Part 4 on smith: take the command skep-setup check prints, run it as
# written, see setup and the agent say enabled, then restore the exact
# file state (smith had no /usr/local/etc/pkg at all).
set -u
echo "before: $(ls -ld /usr/local/etc/pkg 2>&1)"
cmd=$(/usr/local/sbin/skep-setup check -mgmt vtnet0 | sed -n 's/^ *\$ \(mkdir -p \/usr\/local\/etc\/pkg\/repos.*\)$/\1/p')
[ -n "$cmd" ] || { echo "no printed command"; exit 1; }
echo "printed command: $cmd"
sh -c "$cmd"; echo "ran it: exit $?"
ls -la /usr/local/etc/pkg/repos; echo "--- FreeBSD.conf:"; cat /usr/local/etc/pkg/repos/FreeBSD.conf
pkg -vv | grep -A2 "FreeBSD-base:" 
echo "--- skep-setup check after:"
/usr/local/sbin/skep-setup plan -mgmt vtnet0 | grep -A1 "FreeBSD-base"
