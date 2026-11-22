#!/bin/sh
#
# skep package: pre-deinstall script (ADR 0012 Decision 1, answer 14).
#
# Refuses (exit 1, so pkg stops) while Skep guests run on this host:
# jails named skep-* (jls) and bhyve VMs named skep-* (/dev/vmm). Deleting
# the package then would leave them running with nothing to manage them.
# "pkg delete -f" still overrides it, as for any package.
#
# It never refuses during an upgrade. pkg runs the old package's
# deinstall scripts when it upgrades, and sets PKG_UPGRADE for them; an
# upgrade replaces the files and leaves running guests alone (ADR 0012
# Decision 5). Pending FreeBSD validation: that pkg sets PKG_UPGRADE (any
# non-empty value counts) is to be confirmed on a lab VM.
#
# It changes nothing: no service is stopped, no guest is touched.

set -u

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
	echo "skep: refusing to remove the package while Skep guests are running:" >&2
	for g in ${jails}; do echo "skep:   jail ${g}" >&2; done
	for g in ${vms}; do echo "skep:   VM ${g}" >&2; done
	echo "skep: stop them first (web interface, skepctl, or the API), or run skep-setup remove --plan." >&2
	echo "skep: pkg delete -f overrides this check." >&2
	exit 1
fi
exit 0
