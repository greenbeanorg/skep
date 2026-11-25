#!/bin/bash
# 8c-1 lab cases a–h (job 5), driven from merrick with 8c1-lab.sh and
# 8c1-api.py. Run each case on its own: 8c1-cases.sh CASE. The roles are
# read again at the start of each case: L the leader, R and X the
# followers (R the one with the lower address).
#
# Environment: LAB8C1_STATE (the token file in the scratchpad), LAB8C1_PW
# (a 0600 file in the scratchpad holding the temporary user's password;
# made by "8c1-cases.sh pw", deleted by "8c1-cases.sh cleanup"), ADMIN (the
# cluster administrator's username after the import) and, for case a,
# ADMIN_<host> (each host's own administrator before it). No password is
# printed; the creds files are only passed by name to 8c1-api.py.
set -u
D=$(dirname "$0")
lab() { "$D/8c1-lab.sh" "$@"; }
api() { python3 "$D/8c1-api.py" "$@"; }
S=10.79.20.81; LG=10.79.20.82; T=10.79.20.83
name() { case $1 in $S) echo smith;; $LG) echo langrishe;; $T) echo star;; esac; }
h() { echo; echo "=== $(date -u +%H:%M:%SZ) $* ==="; }
CREDS=~/skep-lab-creds
TMPU=${TMPU:-lab8c-$(cat "${LAB8C1_STATE%/*}/tmpuser" 2>/dev/null)}
roles() {
	L=$(lab leader); R=""; X=""
	for x in $S $LG $T; do [ "$x" = "$L" ] && continue; if [ -z "$R" ]; then R=$x; else X=$x; fi; done
	echo "roles: L=$(name $L) R=$(name $R) X=$(name $X) ($(lab term $L))"
	Ln=$(name $L); Rn=$(name $R); Xn=$(name $X)
}
uid_of() { api users "$1" "$ADMIN" | awk -v u="$2" '$2 == u {print $1}'; }

case ${1:-} in
pw)
	umask 077
	head -c 24 /dev/urandom | base64 | tr -d '/+=\n' > "$LAB8C1_PW"
	head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n' > "${LAB8C1_STATE%/*}/tmpuser"
	echo "temporary user lab8c-$(cat "${LAB8C1_STATE%/*}/tmpuser"), password in a 0600 file (not shown)" ;;
a)
	h "case a: before the import"
	for x in smith langrishe star; do
		api health $x
		v=ADMIN_$x; api login $x "${!v}" "$CREDS/$x-admin"
		api create $x "${!v}" lab8c-refused "$LAB8C1_PW" viewer
	done ;;
b)
	h "case b: import on smith, then langrishe and star"
	for x in $S $LG $T; do
		h "$(name $x): skep-api admin import-users --yes"
		ssh -n -o BatchMode=yes aba@$x 'sudo skep-api admin import-users --yes; echo exit=$?'
	done
	for x in smith langrishe star; do api login $x "$ADMIN" "$CREDS/smith-admin"; api users $x "$ADMIN" | sort > "${LAB8C1_STATE%/*}/users-$x"; done
	for x in langrishe star; do cmp -s "${LAB8C1_STATE%/*}/users-smith" "${LAB8C1_STATE%/*}/users-$x" && echo "$x: user list equals smith's by ID" || echo "$x: user list DIFFERS"; done
	cat "${LAB8C1_STATE%/*}/users-smith" ;;
c)
	roles
	h "case c: create $TMPU through R ($Rn)"
	api create $Rn "$ADMIN" "$TMPU" "$LAB8C1_PW" viewer
	for x in smith langrishe star; do api users $x "$ADMIN" | grep -c " $TMPU " | sed "s/^/$x has $TMPU: /"; done
	api login $Xn "$TMPU" "$LAB8C1_PW"
	api req $Xn "$TMPU" GET /auth/session
	h "change $TMPU's password on L ($Ln)"
	id=$(uid_of $Ln "$TMPU")
	umask 077; head -c 24 /dev/urandom | base64 | tr -d '/+=\n' > "$LAB8C1_PW.new"
	api setpw $Ln "$ADMIN" "$id" "$LAB8C1_PW.new"
	mv "$LAB8C1_PW.new" "$LAB8C1_PW"
	sleep 2
	api req $Xn "$TMPU" GET /auth/session
	h "disable $TMPU on L; its login is refused on all three"
	api req $Ln "$ADMIN" PUT /users/$id '{"disabled":true}'
	sleep 2
	for x in smith langrishe star; do api login $x "$TMPU" "$LAB8C1_PW"; done
	h "delete $TMPU"
	api req $Ln "$ADMIN" DELETE /users/$id
	sleep 1
	for x in smith langrishe star; do api users $x "$ADMIN" | grep -c " $TMPU " | sed "s/^/$x has $TMPU: /"; done ;;
d)
	roles
	h "case d: create $TMPU, log in on X ($Xn), stop skep_cluster on X"
	api create $Ln "$ADMIN" "$TMPU" "$LAB8C1_PW" viewer
	sleep 2
	api login $Xn "$TMPU" "$LAB8C1_PW"
	lab svc $X skep_cluster stop
	sleep 20
	api health $Xn
	api create $Xn "$ADMIN" lab8c-nq "$LAB8C1_PW" viewer
	api login $Xn "$ADMIN" "$CREDS/smith-admin"
	id=$(uid_of $Ln "$TMPU")
	h "change $TMPU's password on L ($Ln) while X is cut off"
	umask 077; head -c 24 /dev/urandom | base64 | tr -d '/+=\n' > "$LAB8C1_PW.new"
	api setpw $Ln "$ADMIN" "$id" "$LAB8C1_PW.new"
	mv "$LAB8C1_PW.new" "$LAB8C1_PW"
	h "on X the old session still works until X catches up (the stated window)"
	api req $Xn "$TMPU" GET /auth/session
	lab svc $X skep_cluster start
	lab wait3
	sleep 3
	api req $Xn "$TMPU" GET /auth/session
	api health $Xn
	api req $Ln "$ADMIN" DELETE /users/$id ;;
e)
	roles
	h "case e: stop skep_cluster on R ($Rn) and X ($Xn)"
	lab svc $R skep_cluster stop
	lab svc $X skep_cluster stop
	sleep 20
	api health $Ln
	api create $Ln "$ADMIN" lab8c-nq "$LAB8C1_PW" viewer
	api login $Ln "$ADMIN" "$CREDS/smith-admin"
	lab svc $R skep_cluster start
	lab svc $X skep_cluster start
	lab wait3
	sleep 3
	roles
	api login $Ln "$ADMIN" "$CREDS/smith-admin"
	api create $Ln "$ADMIN" "$TMPU" "$LAB8C1_PW" viewer
	sleep 1
	api req $Ln "$ADMIN" DELETE /users/$(uid_of $Ln "$TMPU") ;;
f)
	roles
	h "case f: restart skep_api on R ($Rn): the session and the copy are kept"
	api login $Rn "$ADMIN" "$CREDS/smith-admin"
	ssh -n -o BatchMode=yes aba@$R 'sudo skep-api admin list | wc -l'
	lab svc $R skep_api restart
	sleep 5
	api req $Rn "$ADMIN" GET /auth/session | cut -c1-200
	ssh -n -o BatchMode=yes aba@$R 'sudo skep-api admin list | wc -l'
	api health $Rn ;;
h)
	h "case h: audit verify and the hash count on each node"
	for x in $S $LG $T; do lab audit $x; lab hashes $x; done ;;
cleanup)
	rm -f "$LAB8C1_PW" "$LAB8C1_PW.new" "${LAB8C1_STATE%/*}/tmpuser" "$LAB8C1_STATE" "${LAB8C1_STATE%/*}"/*-cert.pem "${LAB8C1_STATE%/*}"/users-*
	echo "temporary files removed" ;;
*) echo "usage: $0 pw|a|b|c|d|e|f|h|cleanup (case g is run by hand: remove, leave --wipe, join-request, admit, join)" >&2; exit 2 ;;
esac
