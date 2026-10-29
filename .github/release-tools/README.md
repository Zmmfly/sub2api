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

`fork-latest.yml` (main pushes/manual runs) and `fork-sync.yml` (upstream main
merge every 12 hours/manual runs) call the same serialized `fork-build.yml`.
GITHUB_TOKEN pushes do not recursively start workflows: sync explicitly calls
publication with the merge SHA. The former development branch is no longer a
sync or publication target. Merge conflicts stop before pushing; preserve fork
traffic statistics and compression when repairing them.

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
SHA. Normal upstream v-tag publishing remains available outside this fork;
Zmmfly permits that workflow only as a nonpublishing dry run.

Publication never deletes or overwrites release assets. Draft retries verify
existing bytes and upload missing assets. A completed release is hash-verified and reused without recompiling; verified
archives can recover missing images. Incomplete published releases fail closed
for manual investigation.
Versioned images are reused after label/platform verification on retries, while
registry/authentication failures
fail closed rather than treating a tag as absent. Immutable history is retained.
A run interrupted between release publication and latest promotion can be rerun.
No personal access token, DockerHub or notification secret is required.

Additional checks:

```bash
python -m unittest discover -s .github/scripts -p 'test_fork*.py'
bash -n .github/scripts/fork-*.sh
```
