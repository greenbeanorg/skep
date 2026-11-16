#!/bin/sh
# ADR 0010 step 6, deployed run: record the host files the build must not
# change, from outside the agent (the trace's method: type, mode, owner,
# size, mtime of every file and link, SHA-256 of every file) under /etc,
# /var/db/pkg and /usr/share/certs. Nothing is excluded here, so a change
# to /var/db/pkg/vuln.xml would show (and be explained) rather than be
# hidden. Usage: sudo sh -s NAME < hashhost.sh  -> $OUT/host-NAME.txt
set -eu
OUT=/skeptest/step6-scratch/deployed
mkdir -p $OUT
for d in /etc /var/db/pkg /usr/share/certs; do
	find "$d" \( -type f -o -type l \) -print0 | sort -z | xargs -0 stat -f '%HT %Sp %Su:%Sg %z %m %N'
	find "$d" -type f -print0 | sort -z | xargs -0 sha256 -r
done > $OUT/host-$1.txt
echo "host-$1: $(wc -l < $OUT/host-$1.txt) lines sha256 $(sha256 -q $OUT/host-$1.txt)"
