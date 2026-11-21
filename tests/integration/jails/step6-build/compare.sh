#!/bin/sh
# ADR 0010 step 6: compare the Skep-built template with the traced build
# (skeptest/trace-7b, read only). Every entry of each tree: type, mode,
# owner, file flags, size, link target, and SHA-256 of regular files;
# modification times are left out (they always differ).
set -u
NEW=/skeptest/step6/templates/$1
OLD=/skeptest/trace-7b/root
OUT=/skeptest/step6-scratch/out
list() {
	cd "$1" || exit 1
	find -x . -print0 | sort -z | xargs -0 stat -f '%N|%HT|%Sp|%Su:%Sg|%Sf|%z|%Y' | while IFS='|' read -r n t p o f z y; do
		[ "$t" = "Directory" ] && z=-
		h=-; [ "$t" = "Regular File" ] && h=$(sha256 -q "$n")
		echo "$n|$t|$p|$o|$f|$z|$y|$h"
	done
}
list "$NEW" > $OUT/list-new.txt
list "$OLD" > $OUT/list-trace.txt
wc -l $OUT/list-new.txt $OUT/list-trace.txt
cut -d'|' -f1 $OUT/list-new.txt > $OUT/paths-new.txt
cut -d'|' -f1 $OUT/list-trace.txt > $OUT/paths-trace.txt
comm -13 $OUT/paths-trace.txt $OUT/paths-new.txt > $OUT/only-new.txt
comm -23 $OUT/paths-trace.txt $OUT/paths-new.txt > $OUT/only-trace.txt
sort $OUT/list-new.txt > $OUT/s-new; sort $OUT/list-trace.txt > $OUT/s-trace
comm -3 $OUT/s-trace $OUT/s-new | cut -d'|' -f1 | tr -d '\t' | sort -u > $OUT/differ-any.txt
comm -12 $OUT/paths-trace.txt $OUT/paths-new.txt > $OUT/both.txt
comm -12 $OUT/both.txt $OUT/differ-any.txt > $OUT/changed.txt
echo "only in the new template: $(wc -l < $OUT/only-new.txt)"
echo "only in the trace: $(wc -l < $OUT/only-trace.txt)"
echo "in both, different: $(wc -l < $OUT/changed.txt)"
: > $OUT/changed-detail.txt
while read -r p; do
	echo "~ $p" >> $OUT/changed-detail.txt
	grep -F "$p|" $OUT/list-trace.txt | awk -F'|' -v p="$p" '$1==p {print "  trace: " $0}' >> $OUT/changed-detail.txt
	grep -F "$p|" $OUT/list-new.txt | awk -F'|' -v p="$p" '$1==p {print "  new:   " $0}' >> $OUT/changed-detail.txt
done < $OUT/changed.txt
echo "== services.db"
for r in "$OLD" "$NEW" /; do
	f=$r/var/db/services.db
	nz=$(od -An -v -tx1 "$f" | tr -s ' ' '\n' | grep -vc '^00$\|^$')
	echo "$(sha256 -q $f) size=$(stat -f %z $f) nonzero_bytes=$nz $f"
done
