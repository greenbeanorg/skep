#!/bin/sh
# Traced pkgbase template build for ADR 0010 step 6 (run as root on hearst).
# Scratch only: skeptest/trace-7b. Nothing under skeptest/skep is touched.
# Recorded run: results/step6-pkg-trace.txt (2026-11-16). This is the
# script exactly as run; it leaves TMPDIR unset, so pkg wrote its
# extraction temporaries in the host's /tmp (see the results).
# Clean up afterwards with: zfs destroy -r skeptest/trace-7b; rm -r /var/tmp/skep-trace7b
set -eu

DS=skeptest/trace-7b
OUT=/var/tmp/skep-trace7b
MP=/skeptest/trace-7b

hashhost() {
	# Every regular file and symlink under the three trees: type, mode,
	# owner, size, mtime, and SHA-256 of regular files.
	for d in /etc /var/db/pkg /usr/share/certs; do
		find "$d" \( -type f -o -type l \) -print0 | sort -z | \
		    xargs -0 stat -f '%HT %Sp %Su:%Sg %z %m %N'
		find "$d" -type f -print0 | sort -z | xargs -0 sha256 -r
	done
}

mkdir -p "$OUT"
chmod 0700 "$OUT"

zfs list -H -o name "$DS" >/dev/null 2>&1 && { echo "$DS exists, refusing"; exit 1; }
zfs create -o mountpoint="$MP" "$DS"
chmod 0700 "$MP"
mkdir -m 0755 "$MP/root"
mkdir -m 0700 "$MP/work" "$MP/work/cache"

hashhost > "$OUT/host-before.txt"

# Sample the environment of pkg and its children while the build runs.
(
	while :; do
		for p in $(pgrep -f -- '--rootdir /skeptest/trace-7b/root' 2>/dev/null) \
		    $(pgrep -x sh 2>/dev/null) $(pgrep -f 'certctl|pwd_mkdb|services_mkdb|cap_mkdb' 2>/dev/null); do
			procstat -e "$p" 2>/dev/null | grep -E 'pkg|PKG_|certctl|mkdb' || true
		done
		sleep 0.2
	done
) > "$OUT/procstat-e.txt" 2>&1 &
SAMPLER=$!

set +e
ktrace -i -t cn -f "$OUT/build.ktr" \
    env -i PATH=/sbin:/bin:/usr/sbin:/usr/bin ASSUME_ALWAYS_YES=yes \
    /usr/sbin/pkg --rootdir "$MP/root" \
        -o PKG_CACHEDIR="$MP/work/cache" \
        -o PKG_DBDIR="$MP/root/var/db/pkg" \
        install -y FreeBSD-set-minimal > "$OUT/build.log" 2>&1
RC=$?
set -e
kill "$SAMPLER" 2>/dev/null || true
echo "pkg exit: $RC" | tee -a "$OUT/build.log"

hashhost > "$OUT/host-after.txt"
if cmp -s "$OUT/host-before.txt" "$OUT/host-after.txt"; then
	echo "HOST STATE: identical"
else
	echo "HOST STATE: CHANGED"
	diff "$OUT/host-before.txt" "$OUT/host-after.txt" | head -50
fi
wc -l "$OUT/host-before.txt"
ls -la "$OUT"
