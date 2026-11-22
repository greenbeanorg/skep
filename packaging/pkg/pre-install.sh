#!/bin/sh
#
# skep package: pre-install script (ADR 0012 Decision 1).
#
# Creates the skep group and user with the fixed UID and GID 409 when
# neither exists. It changes nothing else: it never edits rc.conf, never
# enables or starts a service, and never touches an existing account.
#
# Refuses (exit 1, so pkg stops the install) when:
#   - only one of the skep user and the skep group exists;
#   - neither exists and UID 409 or GID 409 belongs to another name.
# Warns, and continues, when both exist with other numbers (hosts set up
# by hand before ADR 0012: they work on a single host).

set -u

SKEP_NAME="skep"
SKEP_ID="409"

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

user_line=$(pw_cmd usershow "${SKEP_NAME}" 2>/dev/null || true)
group_line=$(pw_cmd groupshow "${SKEP_NAME}" 2>/dev/null || true)

if [ -n "${user_line}" ] && [ -n "${group_line}" ]; then
	uid=$(field "${user_line}" 3)
	gid=$(field "${group_line}" 3)
	if [ "${uid}" != "${SKEP_ID}" ] || [ "${gid}" != "${SKEP_ID}" ]; then
		echo "skep: warning: the skep account exists with UID ${uid} and GID ${gid}, not ${SKEP_ID}." >&2
		echo "skep: it works on a single host; a cluster will need ${SKEP_ID} on every node. Nothing was changed." >&2
	fi
	exit 0
fi

if [ -n "${user_line}" ] || [ -n "${group_line}" ]; then
	echo "skep: refusing to install: only one of the skep user and the skep group exists." >&2
	echo "skep: something other than this package made it; resolve it by hand (pw(8)) and install again." >&2
	exit 1
fi

other_user=$(pw_cmd usershow -u "${SKEP_ID}" 2>/dev/null || true)
other_group=$(pw_cmd groupshow -g "${SKEP_ID}" 2>/dev/null || true)
if [ -n "${other_user}" ]; then
	echo "skep: refusing to install: UID ${SKEP_ID} belongs to the user $(field "${other_user}" 1)." >&2
	exit 1
fi
if [ -n "${other_group}" ]; then
	echo "skep: refusing to install: GID ${SKEP_ID} belongs to the group $(field "${other_group}" 1)." >&2
	exit 1
fi

pw_cmd groupadd "${SKEP_NAME}" -g "${SKEP_ID}" || exit 1
if ! pw_cmd useradd "${SKEP_NAME}" -u "${SKEP_ID}" -g "${SKEP_NAME}" \
	-d /nonexistent -s /usr/sbin/nologin -c "Skep service account"; then
	pw_cmd groupdel "${SKEP_NAME}" >/dev/null 2>&1
	exit 1
fi
echo "skep: created the skep group and user (UID and GID ${SKEP_ID}, no login shell, no home)."
exit 0
