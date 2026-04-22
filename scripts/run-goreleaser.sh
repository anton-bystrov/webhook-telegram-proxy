#!/usr/bin/env sh
set -eu

if command -v goreleaser >/dev/null 2>&1; then
	exec goreleaser "$@"
fi

if command -v docker >/dev/null 2>&1; then
	exec docker run --rm \
		-e GITHUB_TOKEN \
		-e GORELEASER_CURRENT_TAG \
		-v "$PWD":/workspace \
		-w /workspace \
		goreleaser/goreleaser:v2.8.1 \
		"$@"
fi

echo "goreleaser or docker is required to run release tasks" >&2
exit 1
