#!/bin/sh
# Analyse the ktrace of the traced pkgbase build (run as root on hearst).
# Usage: sudo sh pkgbase-trace-analyze.sh  (after pkgbase-trace.sh)
set -eu
OUT=/var/tmp/skep-trace7b
ROOT=/skeptest/trace-7b/root
WORK=/skeptest/trace-7b/work
K=$OUT/kdump.txt

kdump -f "$OUT/build.ktr" -t cn > "$K" 2>/dev/null
echo "kdump lines: $(wc -l < "$K")"

echo "== processes (pid comm), in order of appearance =="
awk '!seen[$1]++ {print $1, $2}' "$K"

echo "== every execve/fexecve: pid comm path =="
awk '$3=="CALL" && ($4 ~ /^execve/ || $4 ~ /^fexecve/) {want[$1]=1; next}
     $3=="NAMI" && want[$1] {print $1, $2, $4; want[$1]=0}' "$K"

echo "== writes outside the root and the work area =="
# A CALL of open/openat with O_WRONLY/O_RDWR/O_CREAT/O_TRUNC, or a
# path-modifying call, followed by its NAMI line(s).
awk -v root="$ROOT" -v work="$WORK" '
  $3=="CALL" {
    w[$1]=0
    if ($0 ~ /open(at)?\(/ && $0 ~ /O_WRONLY|O_RDWR|O_CREAT|O_TRUNC|O_APPEND/) w[$1]=1
    if ($4 ~ /^(unlink|unlinkat|rename|renameat|mkdir|mkdirat|rmdir|link|linkat|symlink|symlinkat|chmod|fchmodat|chown|fchownat|lchown|chflags|chflagsat|lchflags|utimensat|utimes|lutimes|truncate|mknod|mknodat|mkfifo|mkfifoat)\(/) w[$1]=1
    c=$4; sub(/\(.*/, "", c); call[$1]=c
    next
  }
  $3=="NAMI" && w[$1] {
    p=$4; gsub(/"/, "", p)
    if (index(p, root) == 1 || index(p, work) == 1) next
    if (p !~ /^\//) { rel[$1" "$2" "call[$1]]++; next }
    print $1, $2, call[$1], p
  }
  END { for (r in rel) print "RELATIVE(" rel[r] ")", r }
' "$K" | sort | uniq -c | sort -rn | head -80

echo "== absolute host paths read by script processes (sh and children) =="
awk '!seen[$1]++ {comm[$1]=$2}
     $3=="NAMI" && ($2=="sh" || $2 ~ /mkdb|certctl|openssl|cap_mkdb/) {
       p=$4; gsub(/"/, "", p); if (p ~ /^\//) print $2, p }' "$K" \
  | grep -v -e "$ROOT" | sort | uniq -c | sort -rn | head -60

echo "== template root: deferred triggers, cache, size =="
ls -la "$ROOT/var/db/pkg/" "$ROOT/var/db/pkg/triggers" 2>&1 | head -30 || true
for f in "$ROOT"/var/db/pkg/triggers/*; do [ -f "$f" ] && { echo "--- $f"; head -20 "$f"; }; done
ls -la "$ROOT/var/cache/pkg" 2>&1 || true
du -sh "$WORK/cache" "$ROOT"
chroot "$ROOT" /bin/freebsd-version -u
ls -la "$ROOT/etc/pwd.db" "$ROOT/etc/spwd.db" "$ROOT/var/db/services.db" "$ROOT/etc/login.conf.db" 2>&1 || true
ls "$ROOT/etc/ssl/certs" | wc -l
echo "== procstat -e samples (unique lines) =="
sort -u "$OUT/procstat-e.txt" | head -40
echo "== build log, script/trigger lines =="
grep -n -i -E "trigger|warning|error" "$OUT/build.log" | head -30 || true
echo "== host files changed during the build (UFS root), outside the trace output =="
find / -xdev -newer "$OUT/host-before.txt" ! -newer "$OUT/host-after.txt" \
    ! -path "$OUT*" ! -path "/var/log/*" 2>/dev/null | head -40
