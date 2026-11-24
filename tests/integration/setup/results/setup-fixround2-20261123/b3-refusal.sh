#!/bin/sh
# B3(b) on smith: every command refuses a group-writable /var/db/skep/setup.
# No command is given -yes, and stdin is empty, so none can proceed.
set -u
S=/var/tmp/skep-b2/skep-setup
D=/var/db/skep/setup
M=$(stat -f %Lp "$D")
O=$(stat -f %Su:%Sg "$D")
trap 'chmod "$M" "$D"; echo "restored: $(stat -f "%Lp %Su:%Sg" "$D") (was $M $O)"' EXIT
echo "before: $M $O"
chmod g+w "$D"
echo "now: $(stat -f %Lp "$D")"
for c in "check -mgmt vtnet0" "plan -mgmt vtnet0" "apply -mgmt vtnet0" "rollback" "upgrade" "remove" "remove -plan"; do
  out=$("$S" $c </dev/null 2>&1); rc=$?
  echo "[$c] exit=$rc: $(printf '%s' "$out" | tail -1 | cut -c1-120)"
done
