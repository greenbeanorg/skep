#!/bin/sh
#
# skep package: pre-deinstall script (ADR 0012 Decision 1, answer 14).
#
# Warns, and exits 1, while Skep guests run on this host: jails named
# skep-* (jls) and bhyve VMs named skep-* (/dev/vmm). Deleting the package
# then leaves them running with nothing to manage them.
#
# The exit status does not stop pkg: pkg 2.9.2 (measured on both lab VMs,
# 2026-11-22) prints "DEINSTALL script failed" and deletes the files
# anyway unless DEVELOPER_MODE is on. So this is a warning, and says so;
# the guard for Skep 0.1 is "skep-setup remove", which refuses while
# guests exist (ADR 0012, amendment on the pre-deinstall guard). Exit 1 is
# kept so that a pkg that does honour it stops.
#
# It never refuses during an upgrade. pkg runs the old package's
# deinstall scripts when it upgrades, and sets PKG_UPGRADE for them; an
# upgrade replaces the files and leaves running guests alone (ADR 0012
# Decision 5). Pending FreeBSD validation: that pkg sets PKG_UPGRADE (any
# non-empty value counts) is to be confirmed on a lab VM.
#
# It changes nothing: no service is stopped, no guest is touched.

set -u

# A fixed PATH: pkg runs this script, as root, with the caller's
# environment. (The package tests replace this one line to find fakes.)
PATH=/sbin:/bin:/usr/sbin:/usr/bin
export PATH

if [ -n "${PKG_UPGRADE:-}" ]; then
	exit 0
fi

# A package installed into another root (pkg -r) is not this host's.
if [ -n "${PKG_ROOTDIR:-}" ] && [ "${PKG_ROOTDIR}" != "/" ]; then
	exit 0
fi

jails=$(jls -N name 2>/dev/null | grep '^skep-' || true)
# SKEP_VMM_DIR exists for the package tests only.
vmm_dir="${SKEP_VMM_DIR:-/dev/vmm}"
vms=""
if [ -d "${vmm_dir}" ]; then
	vms=$(ls "${vmm_dir}" 2>/dev/null | grep '^skep-' || true)
fi

if [ -n "${jails}" ] || [ -n "${vms}" ]; then
	echo "skep: WARNING: Skep guests are running:" >&2
	for g in ${jails}; do echo "skep:   jail ${g}" >&2; done
	for g in ${vms}; do echo "skep:   VM ${g}" >&2; done
	echo "skep: pkg deletes the files anyway (it does not stop on this script's failure)," >&2
	echo "skep: and leaves these guests running with nothing to manage them." >&2
	echo "skep: Before pkg delete, stop them (web interface, skepctl, or the API) and run" >&2
	echo "skep: skep-setup remove, which refuses while Skep guests exist." >&2
	exit 1
fi
exit 0
