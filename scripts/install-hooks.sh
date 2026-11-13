#!/bin/sh
#
# Installs Skep's git hooks into this clone. Run it as "make install-hooks".
#
# The hooks are linked, not copied, so they follow the repository. An
# existing hook that is not a link is left alone: this script never
# replaces a file somebody put there by hand.

set -eu

top=$(git rev-parse --show-toplevel)
hooks=$(git rev-parse --git-path hooks)
case "${hooks}" in
/*) ;;
*) hooks="${top}/${hooks}" ;;
esac
mkdir -p "${hooks}"

for name in pre-push; do
	source="${top}/scripts/hooks/${name}"
	target="${hooks}/${name}"
	if [ ! -x "${source}" ]; then
		echo "install-hooks: ${source} is missing or not executable" >&2
		exit 1
	fi
	if [ -e "${target}" ] && [ ! -L "${target}" ]; then
		echo "install-hooks: ${target} exists and is not a link; move it away first" >&2
		exit 1
	fi
	ln -sf "${source}" "${target}"
	echo "installed ${target} -> scripts/hooks/${name}"
done
