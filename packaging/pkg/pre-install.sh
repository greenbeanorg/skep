#!/bin/sh
#
# skep package: pre-install script (ADR 0012 Decision 1, ADR 0013 B1).
#
# Creates the two service accounts with their fixed numbers when they do
# not exist: the skep group and user (UID and GID 409, skep-api) and the
# skep-cluster group and user (UID and GID 410, the cluster service). It
# changes nothing else: it never edits rc.conf, never enables or starts a
# service, and never touches an existing account.
#
# For each account it refuses (exit 1, so pkg stops the install) when:
#   - only one of the user and the group exists;
#   - neither exists and the UID or the GID belongs to another name.
# It warns, and continues, when both exist with other numbers (hosts set
# up by hand before ADR 0012: they work on a single host).
#
# The skep account is dealt with first. If it is created and the
# skep-cluster account is then refused, it stays: the next install finds
# it and goes on.

set -u

# A fixed PATH: pkg runs this script, as root, with the caller's
# environment. (The package tests replace this one line to find fakes.)
PATH=/sbin:/bin:/usr/sbin:/usr/bin
export PATH

pw_cmd() {
	if [ -n "${PKG_ROOTDIR:-}" ] && [ "${PKG_ROOTDIR}" != "/" ]; then
		pw -R "${PKG_ROOTDIR}" "$@"
	else
		pw "$@"
	fi
}

# field N of a colon-separated line
field() {
	echo "$1" | cut -d: -f"$2"
}

# ensure_account NAME ID COMMENT
ensure_account() {
	acct_name="$1"
	acct_id="$2"
	acct_comment="$3"

	user_line=$(pw_cmd usershow "${acct_name}" 2>/dev/null || true)
	group_line=$(pw_cmd groupshow "${acct_name}" 2>/dev/null || true)

	if [ -n "${user_line}" ] && [ -n "${group_line}" ]; then
		uid=$(field "${user_line}" 3)
		gid=$(field "${group_line}" 3)
		if [ "${uid}" != "${acct_id}" ] || [ "${gid}" != "${acct_id}" ]; then
			echo "skep: warning: the ${acct_name} account exists with UID ${uid} and GID ${gid}, not ${acct_id}." >&2
			echo "skep: it works on a single host; a cluster will need ${acct_id} on every node. Nothing was changed." >&2
		fi
		return 0
	fi

	if [ -n "${user_line}" ] || [ -n "${group_line}" ]; then
		echo "skep: refusing to install: only one of the ${acct_name} user and the ${acct_name} group exists." >&2
		echo "skep: something other than this package made it; resolve it by hand (pw(8)) and install again." >&2
		return 1
	fi

	other_user=$(pw_cmd usershow -u "${acct_id}" 2>/dev/null || true)
	other_group=$(pw_cmd groupshow -g "${acct_id}" 2>/dev/null || true)
	if [ -n "${other_user}" ]; then
		echo "skep: refusing to install: UID ${acct_id} belongs to the user $(field "${other_user}" 1)." >&2
		return 1
	fi
	if [ -n "${other_group}" ]; then
		echo "skep: refusing to install: GID ${acct_id} belongs to the group $(field "${other_group}" 1)." >&2
		return 1
	fi

	pw_cmd groupadd "${acct_name}" -g "${acct_id}" || return 1
	if ! pw_cmd useradd "${acct_name}" -u "${acct_id}" -g "${acct_name}" \
		-d /nonexistent -s /usr/sbin/nologin -c "${acct_comment}"; then
		pw_cmd groupdel "${acct_name}" >/dev/null 2>&1
		return 1
	fi
	echo "skep: created the ${acct_name} group and user (UID and GID ${acct_id}, no login shell, no home)."
	return 0
}

ensure_account "skep" "409" "Skep service account" || exit 1
ensure_account "skep-cluster" "410" "Skep cluster service account" || exit 1
exit 0
