#!/bin/sh
R=/skeptest/step6-probe/root; T=/skeptest/trace-7b/root
nz() { od -An -v -tx1 "$1" | tr -s ' ' '\n' | grep -vc '^00$\|^$'; }
echo "non-zero bytes: trace=$(nz $T/var/db/services.db) jail(/etc/services)=$(nz $R/var/db/services.db) host=$(nz /var/db/services.db)"
echo "host /var/db/services.db: $(sha256 -q /var/db/services.db) $(stat -f '%z %Sm' /var/db/services.db)"
grep -c '^[a-z]' $R/etc/services
pkg query '%n %v' FreeBSD-runtime
pkg info --raw --raw-format json-compact FreeBSD-runtime | grep -o '"post-install":"[^"]*"'
