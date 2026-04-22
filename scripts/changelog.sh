#!/usr/bin/env sh
set -eu

output_path="${1:-dist/CHANGELOG.next.md}"
mkdir -p "$(dirname "${output_path}")"

last_tag="$(git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)"
range_spec="HEAD"
header="Repository history"

if [ -n "${last_tag}" ]; then
	range_spec="${last_tag}..HEAD"
	header="Changes since ${last_tag}"
fi

{
	printf '# Draft changelog\n\n'
	printf 'Generated on %s.\n\n' "$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
	printf '## %s\n\n' "${header}"
	if git log --pretty=format:'- %s (%h)' ${range_spec} 2>/dev/null | grep . >/dev/null 2>&1; then
		git log --pretty=format:'- %s (%h)' ${range_spec}
		printf '\n'
	else
		printf -- '- No changes found.\n'
	fi
} > "${output_path}"

printf 'Wrote %s\n' "${output_path}"
