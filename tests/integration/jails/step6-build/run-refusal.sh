#!/bin/sh
# ADR 0010 step 6: a changed trigger file is refused with
# template_script_unknown and the build leaves nothing behind.
set -u
S=/skeptest/step6-scratch
OUT=$S/out
cd $S || exit 1
zfs list -H -o name -r skeptest/step6 > $OUT/datasets-before-refusal.txt
env -i PATH=/sbin:/bin:/usr/sbin:/usr/bin TMPDIR=$S/tmp SKEP_IT_TEMPLATE_POOL=skeptest SKEP_IT_TEMPLATE_ROOT=step6 \
    ./templates.test -test.v -test.run 'TestIntegrationRefusal$' > $OUT/refusal.log 2>&1
echo "test exit $?"
zfs list -H -o name -r skeptest/step6 > $OUT/datasets-after-refusal.txt
cmp -s $OUT/datasets-before-refusal.txt $OUT/datasets-after-refusal.txt && echo "datasets unchanged by the refused build" || diff $OUT/datasets-before-refusal.txt $OUT/datasets-after-refusal.txt
ls -A /skeptest/step6/templates/
echo "jails: $(jls name | grep -c .)"
