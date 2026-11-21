#!/bin/sh
# ADR 0010 step 6: the native template build on hearst, with the outside
# checks of decision 16. Run as `ssh hearst 'sudo sh -s' < run-build.sh`
# after the test binaries are in $S. Everything it writes is in the
# skeptest datasets skeptest/step6-scratch and skeptest/step6.
set -u
S=/skeptest/step6-scratch
OUT=$S/out
mkdir -p $OUT; chmod 0700 $OUT
cd $S || exit 1

hashhost() {
	for d in /etc /var/db/pkg /usr/share/certs; do
		find "$d" \( -type f -o -type l \) -print0 | sort -z | \
		    xargs -0 stat -f '%HT %Sp %Su:%Sg %z %m %N'
		find "$d" -type f -print0 | sort -z | xargs -0 sha256 -r
	done
}

echo "== host before"; hashhost > $OUT/host-before.txt; wc -l < $OUT/host-before.txt
touch $OUT/sampling
( while [ -e $OUT/sampling ]; do
	for p in $(pgrep -x pkg 2>/dev/null) $(pgrep -x jexec 2>/dev/null); do procstat -e $p 2>/dev/null | tail -n +2; done
	sleep 0.2
  done ) > $OUT/procstat-raw.txt 2>&1 &

echo "== build"
env -i PATH=/sbin:/bin:/usr/sbin:/usr/bin TMPDIR=$S/tmp SKEP_IT_TEMPLATE_POOL=skeptest SKEP_IT_TEMPLATE_ROOT=step6 \
    ./templates.test -test.v -test.run 'TestIntegrationBuild$' > $OUT/build.log 2>&1
echo "test exit $?"
rm -f $OUT/sampling; sleep 1
sort -u $OUT/procstat-raw.txt | sed 's/^ *[0-9]* //' | sort -u > $OUT/procstat.txt

echo "== host after"; hashhost > $OUT/host-after.txt
sha256 -q $OUT/host-before.txt $OUT/host-after.txt
cmp -s $OUT/host-before.txt $OUT/host-after.txt && echo "HOST STATE: identical" || { echo "HOST STATE: DIFFERENT"; diff $OUT/host-before.txt $OUT/host-after.txt | head -20; }
echo "== files changed on the UFS root during the build (outside skeptest, /var/log)"
find / -xdev -newer $OUT/host-before.txt ! -newer $OUT/host-after.txt ! -path '/var/log/*' ! -path '/skeptest*' 2>/dev/null | head -20
echo "== jails"; jls name | grep -c . ; jls name
echo "== datasets"; zfs list -r -o name,used,readonly,mountpoint skeptest/step6
ls -la /skeptest/step6/templates/
