# Installing

`email` is one static binary for Linux and macOS (amd64, arm64). Pick one way:

```sh
# Script: picks your platform, checks checksums.txt, puts email in ~/.local/bin
# (/usr/local/bin as root). Rerunning with the same version changes nothing.
curl -fsSL https://github.com/james-tronai/email-cli/releases/latest/download/install.sh | sh
curl -fsSL https://github.com/james-tronai/email-cli/releases/latest/download/install.sh | sh -s -- --version v0.10.2 --dest /usr/local/bin   # a fixed version

# Homebrew (macOS, Linux)
brew install james-tronai/tap/email

# Container (linux/amd64, linux/arm64; FROM scratch, entrypoint `email`)
docker run --rm -v email-data:/data ghcr.io/james-tronai/email-cli:latest version   # or a release tag, e.g. :v0.10.2

# From source (Go 1.27+)
go install github.com/james-tronai/email-cli/cmd/email@latest
```

- **Options**: `install.sh --help` lists them. `--verify` also checks the release's keyless cosign signature, and needs `cosign`. No GitHub token is needed.
- **PATH**: the script says so when `~/.local/bin` is not on your PATH.
- **Kubernetes initContainer**: the image has no shell or `cp`. Use `email self-install --dest /opt/data/.local/bin` to copy the binary onto a shared volume. The [Helm chart](../charts/hermes-email/README.md) does this for you.
- **Updates**: `email version --check` says whether a newer release exists. It never downloads anything. Rerun the install command to update.
- **Release assets**: `email_<os>_<arch>` (bare binaries), `checksums.txt`, `checksums.txt.sigstore.json`, one SBOM per binary, `install.sh`, and the chart `hermes-email-<version>.tgz`. These names are stable ([compatibility](reference.md#compatibility)). How releases are cut and verified: [releasing.md](releasing.md).

## Files

| path | contents |
| --- | --- |
| `~/.config/email/` (`$EMAIL_CONFIG_DIR`) | `config.toml` (`default_account`), `policy.toml`, `accounts/<alias>/` (0700; tokens and `imap-credentials.json` 0600) |
| `~/.local/state/email/` (`$EMAIL_STATE_DIR`) | `audit.jsonl`, `sync/<alias>-<label>.cursor`, `send/` (idempotency records), `watch/<alias>.json` (watch's own cursor and notified ids) |

`$EMAIL_POLICY_FILE` points at a policy elsewhere (the Helm chart mounts it read-only). To uninstall, stop any watch service (`systemctl --user disable --now email-watch`, or `launchctl bootout gui/$(id -u)/io.github.james-tronai.email-watch`), then delete the binary and these two directories. Revoke the app's access on the provider side too (Entra: Enterprise applications; Google: Third-party access; app passwords: delete the password).
