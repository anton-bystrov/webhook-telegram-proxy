#!/usr/bin/env sh
set -eu

if git describe --tags --exact-match --match 'v*' >/dev/null 2>&1; then
	git describe --tags --exact-match --match 'v*'
	exit 0
fi

short_sha="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"

if last_tag="$(git describe --tags --abbrev=0 --match 'v*' 2>/dev/null)"; then
	printf '%s-dev.%s\n' "${last_tag}" "${short_sha}"
else
	printf 'v0.0.0-dev.%s\n' "${short_sha}"
fi
