# Releasing

This project uses tag-driven releases with Semantic Versioning.

## Versioning

- Create release tags in the form `vX.Y.Z`.
- Keep `CHANGELOG.md` updated before tagging.
- Local non-tag builds use a development version such as `v0.1.0-dev.<sha>`.

## Commit message guidance

Conventional commits are recommended because they produce cleaner release notes:

- `feat: ...`
- `fix: ...`
- `docs: ...`
- `chore: ...`
- `test: ...`

If older commits do not follow that format, releases still work; the changelog draft simply falls back to raw commit subjects.

## Local release preparation

From the repository root:

```bash
make fmt
make test
make vet
make changelog
make release-check
make snapshot
```

`make snapshot` creates versioned archives and Linux packages in `dist/` without publishing anything.

## Publishing a release

1. Update `CHANGELOG.md`.
2. Commit the changelog and any release notes.
3. Create an annotated tag:

```bash
git tag -a v0.1.0 -m "Release v0.1.0"
git push origin v0.1.0
```

4. GitHub Actions will:
   - run formatting, tests, and vet;
   - build binaries for supported platforms;
   - build `.deb` and `.rpm` packages;
   - publish a GitHub Release with checksums and artifacts;
   - publish multi-platform Docker images to `ghcr.io/anton-bystrov/webhook-telegram-proxy`.

## Docker image tags

Tag-driven releases publish:

- `ghcr.io/anton-bystrov/webhook-telegram-proxy:<version>`
- `ghcr.io/anton-bystrov/webhook-telegram-proxy:<major>.<minor>`
- `ghcr.io/anton-bystrov/webhook-telegram-proxy:<major>`
- `ghcr.io/anton-bystrov/webhook-telegram-proxy:latest` for stable releases
