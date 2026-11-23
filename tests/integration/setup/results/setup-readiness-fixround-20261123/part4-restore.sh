#!/bin/sh
set -u
c=$(cat /usr/local/etc/pkg/repos/FreeBSD.conf)
[ "$c" = "FreeBSD-base: { enabled: yes }" ] || { echo "unexpected content, not removing: $c"; exit 1; }
[ "$(ls -A /usr/local/etc/pkg/repos)" = "FreeBSD.conf" ] && [ "$(ls -A /usr/local/etc/pkg)" = "repos" ] || { echo "other files present, not removing"; exit 1; }
rm /usr/local/etc/pkg/repos/FreeBSD.conf && rmdir /usr/local/etc/pkg/repos /usr/local/etc/pkg
echo "after restore: $(ls -ld /usr/local/etc/pkg 2>&1)"
pkg -vv | grep -A2 "FreeBSD-base:"
/usr/local/sbin/skep-setup plan -mgmt vtnet0 | grep -A1 "FreeBSD-base"
