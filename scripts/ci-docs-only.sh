#!/bin/sh
# Reads changed paths, one per line, on stdin. Exits 0 when there is at
# least one and every one is documentation or a recorded test result, so
# CI may skip its checks (.github/workflows/ci.yml, job "changes");
# exits 1 otherwise.
#
# Documentation and results only:
#   docs/...                       documentation
#   *.md at the repository root    README.md, CLAUDE.md
#   tests/....md                   test plans and runbooks
#   tests/.../results/...          recorded results (logs, scripts kept as
#                                  evidence; CI never runs them)
# Anything else counts as code. Markdown elsewhere is code on purpose: for
# example scripts/notices/vendored/*/LICENSE.md is read by
# "make notices-check".
n=0
while IFS= read -r f; do
	[ -n "$f" ] || continue
	n=$((n + 1))
	case "$f" in
	*.go) exit 1 ;; # Go anywhere is built by "go vet ./...", even under results/
	docs/*) ;;
	*/*.md) case "$f" in tests/*) ;; *) exit 1 ;; esac ;;
	*.md) ;;
	tests/*/results/*) ;;
	*) exit 1 ;;
	esac
done
[ "$n" -gt 0 ]
