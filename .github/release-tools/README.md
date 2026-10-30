# Release matrix

The release workflow builds the frontend once, then runs each configured Go target on its own Linux runner. `CGO_ENABLED=0` permits cross-compilation. The target matrix is read from `.goreleaser.yaml`, including its exclusions; simple releases select Linux amd64 only.

Each build uses GoReleaser OSS in snapshot mode with the selected release version and one target. Archive naming, bundled files, Go flags and release templates remain in the existing GoReleaser configurations. Every archive is accompanied by its source commit, target, version and SHA256. The publishing job verifies the complete matrix before building images or publishing. It uses GoReleaser's `extra_files` support to publish existing archives and checksums, with builds disabled. No Pro license is needed.

Go caches are isolated by target and refreshed on each source commit, with fallback to the preceding target cache. Save uses the original restore key, even if a build hook changes `go.sum`. Matrix jobs upload uniquely named artifacts. The publishing job extracts only the regular Linux binary from each verified archive and restores its executable permission before constructing Docker contexts. QEMU remains limited to runtime-image instructions. DockerHub images are omitted when its credentials are absent; GHCR is always retained. Simple mode still publishes only the amd64 GHCR image and the simple release description.

All build jobs use the commit resolved by `prepare`, including a manual release's selected tag. Helper scripts come from the workflow revision and are passed as a run-local artifact, so older application tags do not need to contain the new scripts. The workflow serializes release runs to prevent simultaneous updates to moving image tags.

## Validate without publication

From a branch containing this workflow:

```bash
gh workflow run release.yml --ref <branch> \
  -f tag=<branch> -f dry_run=true -f simple_release=false
```

A dry run builds all selected archives and both runtime images, verifies artifact provenance and produces the final checksum file. It exports images locally as OCI archives instead of pushing them. It skips registry logins, GitHub Release publication, DockerHub description updates, Telegram notifications and VERSION synchronization. Test the simple path separately with `simple_release=true`.

Dry-run artifacts are available in the Actions run, including `release-dry-run-report`. Compare job start/end times, GoReleaser's build duration and cache restore results. Do not present an initial cold-cache run as a warmed-cache benchmark; publishing network time is not measured by dry runs.

Helper checks:

```bash
python -m pip install -r .github/release-tools/requirements-release.txt
python -m unittest discover -s .github/release-tools -p 'test_release_matrix.py'
bash -n .github/release-tools/release-images.sh
```

## Zmmfly fork main releases

`fork-latest.yml` accepts newly created canonical `z<base>-N` tags and manual
runs, then calls the serialized `fork-build.yml`. Ordinary main pushes do not
start publication. CI and security checks must pass on the exact selected SHA
before building or publishing any release artifacts.

The assistant's external 12-hour maintenance task checks upstream main, resolves
merges while preserving traffic statistics/compression, and creates an immutable
version tag on the resulting fork main commit. That tag push triggers the same
checks and release pipeline. No upstream changes means no new tag or release.
There is no scheduled upstream-sync workflow or sync helper in GitHub Actions;
the weekly security scan is independent and remains enabled. The former
development branch is not a sync or publication target. Never discard fork
changes with blanket ours/theirs conflict resolution or force-push main.

Tag creation must use authorized user/app credentials: tags pushed by the
workflow's built-in `GITHUB_TOKEN` do not start a new tag-push workflow. This
repository does not store a PAT or add an Actions secret for external maintenance.

`backend/cmd/server/VERSION` remains upstream-owned numeric metadata. Allocation
reads it **at the selected commit**, then reserves `z<base>-N` as an annotated tag,
starting at 1 per base and incrementing the highest existing N. Tags are never
moved. Retrying the same commit reuses its reserved version, even after a failed
build; gaps are possible after abandoned reservations. A superseded queued run
is skipped before allocation. One repository-wide workflow concurrency lock
covers allocation, builds and publication. GitHub may replace older pending
runs with newer pending runs; it never cancels the active publisher here.

The exact z version is injected into the embedded VERSION and Go linker flags,
archives and OCI labels. The full configured binary matrix, frontend and both
Linux runtime images are built (no simple/image-only mode). The numeric suffix
is a **stable fork revision**, not a semantic-version prerelease. The helper
publishes a draft only after all archives/checksums and images exist, explicitly
sets prerelease=false, and promotes GitHub/GHCR latest only for the current main
SHA. The obsolete upstream release and CLA workflow entries have been removed;
shared release tools and their tests remain in use.

Publication never deletes or overwrites release assets. Draft retries verify
existing bytes and upload missing assets. A completed release is hash-verified and reused without recompiling; verified
archives can recover missing images. Incomplete published releases fail closed
for manual investigation.
Versioned images are reused after label/platform verification on retries, while
registry/authentication failures
fail closed rather than treating a tag as absent. Immutable history is retained.
A run interrupted between release publication and latest promotion can be rerun.
Publication itself uses the built-in `GITHUB_TOKEN`; no additional repository
secret is required by the release workflow.

Additional checks:

```bash
python -m unittest discover -s .github/scripts -p 'test_fork*.py'
bash -n .github/scripts/fork-*.sh
```
