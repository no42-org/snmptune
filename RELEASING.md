# Releasing

Versions follow [SemVer](https://semver.org/), derived from Conventional Commits: a `feat` commit bumps the minor version, a `fix` the patch, and a `BREAKING CHANGE` footer or `!` the major.

## What a release contains

The `Release` workflow runs on every `vX.Y.Z` tag, after the same quality gates pull requests go through. It produces:

- Archives for linux, darwin and windows on amd64 and arm64, named `snmptune_X.Y.Z_<os>_<arch>.tar.gz` (`.zip` on Windows), each holding the binary, `LICENSE` and `README.md`.
- `sbom.spdx.json`, a software bill of materials generated with syft.
- `checksums.txt` over every artifact, signed with cosign keyless; the signature and certificate travel in the Sigstore bundle `checksums.txt.sigstore.json`.
- SLSA build provenance attestations for every artifact, verifiable with `gh attestation verify`.

Everything lands on a **draft** GitHub Release. No container image is published; the tool is a single binary.

## Cutting a release

1. Ensure CI on `main` is green.
2. Tag the commit: `git tag -a vX.Y.Z -m "vX.Y.Z" && git push origin vX.Y.Z`.
   The binary reports its version from the tag, so there is no manifest to bump.
3. Wait for the `Release` workflow to finish and check the draft has all assets.
4. Write curated notes (Highlights, Breaking changes, Fixes) and publish:
   `gh release edit vX.Y.Z --notes-file notes.md --draft=false`.

Prerelease tags such as `vX.Y.Z-rc1` are marked as prereleases automatically.

Every push to `main` refreshes the rolling `preview` prerelease with binaries named `snmptune_preview_<os>_<arch>`. It is unstable and unsupported.

## Verifying a download

Check the checksum signature, then the checksum of the archive:

    cosign verify-blob --bundle checksums.txt.sigstore.json \
      --certificate-identity-regexp 'https://github.com/no42-org/snmptune/\.github/workflows/release\.yml@.*' \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com \
      checksums.txt
    sha256sum --check --ignore-missing checksums.txt

Verify build provenance of an archive:

    gh attestation verify snmptune_X.Y.Z_linux_amd64.tar.gz --repo no42-org/snmptune

## Changing the pipeline

`RELEASING.md` describes the pipeline as it is. A change to `.github/workflows/release.yml` or `make release-build` updates this file in the same pull request.
