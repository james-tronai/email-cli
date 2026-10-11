# Releases

Each release is a GitHub Release on a `vX.Y.Z` tag. `.github/workflows/release.yml` runs the tests, then [goreleaser](../.goreleaser.yaml).

## Assets

| asset | contents |
| --- | --- |
| `email_linux_amd64`, `email_linux_arm64`, `email_darwin_amd64`, `email_darwin_arm64` | static binaries (`CGO_ENABLED=0`), no version in the name |
| `checksums.txt` | `sha256sum` format, `<hex>  <name>`, covering every asset below |
| `checksums.txt.sigstore.json` | keyless cosign signature of `checksums.txt` (Sigstore bundle), from v0.7.0 |
| `email_<os>_<arch>.sbom.json` | SPDX SBOM per binary, from v0.7.0 |
| `install.sh` | the install script, from v0.7.0 |

- **Contract**: the binary names and the checksum format are relied on by `install.sh`, the Homebrew cask and the k3s initContainer. Change them only with a major version. New assets may be added.
- **Image**: `ghcr.io/james-tronai/email-cli:<tag>` (and `:latest`) is built from the same binaries for linux/amd64 and linux/arm64, and is signed keylessly as well.
- **Homebrew**: the cask `email` is pushed to `james-tronai/homebrew-tap` when the `HOMEBREW_TAP_GITHUB_TOKEN` secret is set.

## Cutting a release

1. Write `.github/release-notes/vX.Y.Z.md`. Say what changed and, in particular, **what a deployment has to change**: policy keys, login flow, scopes, egress, output fields.
2. Commit it, then push the tag: `git tag -a vX.Y.Z -m … && git push origin vX.Y.Z`.
3. The workflow then:
   - builds and signs everything;
   - publishes the release with the notes file;
   - pushes and signs the image;
   - updates the cask;
   - smoke-tests the image (`version`, and `self-install` out of the image).

`task snapshot` builds the binaries, SBOMs and cask locally into `dist/` without publishing.

## Verifying a download

```sh
sha256sum -c --ignore-missing checksums.txt          # shasum -a 256 -c on macOS
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/james-tronai/email-cli/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
cosign verify ghcr.io/james-tronai/email-cli:<tag> \
  --certificate-identity-regexp '^https://github.com/james-tronai/email-cli/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

`install.sh` does the checksum step always, and the signature step with `--verify`.

## Downloading from a private repository

While the repository is private, anonymous requests get 404. `install.sh` handles this when given `$GITHUB_TOKEN` or `--token-file`; a token needs **Contents: read** on this repository. By hand:

1. `GET https://api.github.com/repos/james-tronai/email-cli/releases/tags/<tag>` with `Authorization: Bearer <token>`.
2. From `.assets[]`, take the `url` of the asset by `name`. This is an `https://api.github.com/…/releases/assets/<id>` URL. Do not use `browser_download_url`, which does not accept a token.
3. `GET` that URL with the token and `Accept: application/octet-stream`. The answer is a `302` to a signed storage URL; follow it **without** the token. `curl -L` drops `Authorization` on a cross-host redirect.
