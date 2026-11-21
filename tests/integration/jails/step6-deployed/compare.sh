#!/bin/sh
# ADR 0010 step 6, deployed run: compare the template the deployed agent
# built through the API ($1, a mountpoint) with the test binary's template
# ($2, read only). Every entry of each tree: path, type, mode, owner, file
# flags, size, link target, SHA-256 of regular files. Modification times
# are left out (they always differ). Usage: sudo sh -s NEW OLD < compare.sh
set -u
NEW=$1
OLD=$2
OUT=/skeptest/step6-scratch/deployed
mkdir -p $OUT
list() {
	cd "$1" || exit 1
	find -x . -print0 | sort -z | xargs -0 stat -f '%N|%HT|%Sp|%Su:%Sg|%Sf|%z|%Y' | while IFS='|' read -r n t p o f z y; do
		[ "$t" = "Directory" ] && z=-
		h=-; [ "$t" = "Regular File" ] && h=$(sha256 -q "$n")
		echo "$n|$t|$p|$o|$f|$z|$y|$h"
	done
}
list "$NEW" > $OUT/list-api.txt
list "$OLD" > $OUT/list-step6.txt
echo "entries: api $(wc -l < $OUT/list-api.txt), step6 $(wc -l < $OUT/list-step6.txt)"
cut -d'|' -f1 $OUT/list-api.txt > $OUT/paths-api.txt
cut -d'|' -f1 $OUT/list-step6.txt > $OUT/paths-step6.txt
comm -13 $OUT/paths-step6.txt $OUT/paths-api.txt > $OUT/only-api.txt
comm -23 $OUT/paths-step6.txt $OUT/paths-api.txt > $OUT/only-step6.txt
comm -12 $OUT/paths-step6.txt $OUT/paths-api.txt > $OUT/both.txt
sort $OUT/list-api.txt > $OUT/s-api; sort $OUT/list-step6.txt > $OUT/s-step6
comm -3 $OUT/s-step6 $OUT/s-api | tr -d '\t' | cut -d'|' -f1 | sort -u > $OUT/differ-any.txt
comm -12 $OUT/both.txt $OUT/differ-any.txt > $OUT/changed.txt
echo "only in the API template: $(wc -l < $OUT/only-api.txt)"; cat $OUT/only-api.txt
echo "only in skeptest/step6: $(wc -l < $OUT/only-step6.txt)"; cat $OUT/only-step6.txt
echo "in both, different: $(wc -l < $OUT/changed.txt)"
while read -r p; do
	echo "~ $p"
	awk -F'|' -v p="$p" '$1==p {print "  step6: " $0}' $OUT/list-step6.txt
	awk -F'|' -v p="$p" '$1==p {print "  api:   " $0}' $OUT/list-api.txt
done < $OUT/changed.txt
