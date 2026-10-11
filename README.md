# email

A multi-account mail CLI for self-hosted agents. The agent gets least-privilege, audited access and works in draft-only mode by default. Output is JSON, and the CLI never prompts.

Status: **M7**. Three providers, all with read, draft, organise, sync and send:

| provider | mailbox | login | setup |
| --- | --- | --- | --- |
| `msgraph` | Microsoft 365 (Graph) | browser, OAuth auth code + PKCE (`--manual` for headless) | [docs/microsoft-365-setup.md](docs/microsoft-365-setup.md) |
| `gmail` | Google Workspace (Gmail API) | the same flow, with a GCP Desktop OAuth client | [docs/google-workspace-setup.md](docs/google-workspace-setup.md) |
| `imap` | personal Gmail, iCloud, Fastmail, QQ, 163, Alibaba Mail, Mailu, or any IMAP/SMTP server | address + app password on stdin (`--password-stdin`), `--preset` for the servers | [docs/gmail-personal-setup.md](docs/gmail-personal-setup.md), [docs/imap-presets.md](docs/imap-presets.md) |

All three support list, search, get, thread, attachments, drafts (create/reply/forward/update/delete), move, label, mark, sync and send. `msgraph` has categories for labels and Outlook search syntax (KQL). `gmail` and `imap` on Gmail use Gmail labels and Gmail search syntax. `imap` on another server falls back to folders and plain IMAP `SEARCH`.

After every setup step, `email doctor` checks the install, the policy, each account's login and the watch setup, and says how to fix what fails. It prints no mail content. Policy templates to start from are in [docs/policy/](docs/policy/): `read`, `draft` (recommended), `send-reply-only` and `send-open`.

`email watch` (M7) is a long-running process that waits for new mail (IMAP IDLE, or a sync poll) and POSTs one JSON notification per batch to a webhook, so the agent is only woken when mail arrives. See [docs/watch.md](docs/watch.md).

`email send` (M4) sends an existing draft. It only works when both the policy and the account login are in mode `send`. See [Sending](#sending).

## Quick try (fake mailbox)

```sh
go build -o email ./cmd/email
export EMAIL_CONFIG_DIR=$PWD/.tmp/cfg EMAIL_STATE_DIR=$PWD/.tmp/state
mkdir -p .tmp/cfg && cp docs/policy.example.toml .tmp/cfg/policy.toml   # without a policy file the mode is read
./email auth login -a demo --provider fake --mode draft
./email list --format text
./email get fake-0002 --format text                # hidden-text injection removed
./email draft reply fake-0002 --body "Thanks"
./email send fake-0004                             # exit 4: disabled by policy
```

## Safety model

- **Modes** `read` < `draft` < `send`. The effective mode is the lower of the policy mode and the mode the account logged in with. Login only requests the scopes that mode needs, so a draft-mode M365 token has no `Mail.Send`. Gmail is different: no Gmail scope can create drafts without also being able to send (`gmail.modify` can send), so on Gmail only the CLI's policy gate blocks sending. An `imap` app password can do everything; there the client enforces the mode itself (no changes below `draft`, and no SMTP connection below `send`).
- **Policy file**: `$EMAIL_POLICY_FILE`, else `~/.config/email/policy.toml` (see [docs/policy.example.toml](docs/policy.example.toml)). A missing file means `read`. An unparsable file, or one with unknown keys, stops every command with exit 4.
- **Audit**: every write and every denial is appended to `~/.local/state/email/audit.jsonl`.
- **Untrusted content**:
  - Message bodies always go in `body_text`.
  - Text output wraps bodies in `BEGIN/END UNTRUSTED MESSAGE BODY` fences; a fence marker inside a body is defused.
  - Hidden HTML text (white-on-white, `display:none`, zero font size, …) is stripped and flagged with `body_hidden_text_removed`. When a message has HTML, the text is derived from that HTML rather than from the sender's `text/plain` part, so the agent reads what a human would see.
  - Bodies are truncated at 20 000 characters.
  - Attachments are only saved by an explicit `download`, with sanitized names and mode 0600.
- **Trash and delete** are disabled unless `[delete] enabled = true`. Draft deletion is always allowed.

## Sending

`email send <draft-id>` sends a draft that already exists. There is no command that composes and sends in one step. The body goes out exactly as it is on the server: the signoff was added when the draft was written.

- **When it is available**: only when the policy says `mode = "send"` **and** the account logged in with `--mode send`. Otherwise it exits 4 before any network call.
- **Recipient rules**: checked against the draft as it is on the server at send time (to + cc + bcc), so edits made after the draft was created count. The rules come from `[send]` in the policy file:
  - `allowed_recipient_domains`: every recipient must be in one of these domains. An empty list means no restriction.
  - `max_recipients`: at most this many recipients. 0 means unlimited.
  - `require_reply_context`: the draft must be a reply or forward, meaning its thread holds a message that is not a draft. A brand-new message is refused.
- **Idempotency**: `--idempotency-key` defaults to the draft id. A key that was sent within the last 24 hours returns the first result with `"replayed": true` and sends nothing. While a send with the same key is running, another one exits 5 with `sendInProgress`. Records live in `<state dir>/send/`.
- **Audit**: every send is written to `audit.jsonl` with its recipients and the sent message id. So is every denial and every replay.
- **Enabling it**:
  - Microsoft 365: add `Mail.Send` to the Entra app (with admin consent), log in again with `--mode send`, and set the policy mode to `send`.
  - Gmail: the draft-mode token can already send, so the policy mode is the only switch.
  - imap: log in again with `--mode send` (same app password) and set the policy mode to `send`. SMTP (`smtp.gmail.com:465` for Gmail) is used only then.

## Exit codes

| code | meaning |
| --- | --- |
| 0 | ok |
| 2 | usage error, or the provider lacks the capability |
| 3 | authentication required (no account, account lock mismatch, expired token) |
| 4 | denied by policy |
| 5 | provider error (`provider_code` carries the upstream code) |
| 6 | timeout |

Errors are printed to stderr as `{"error":{"exit_code":4,"code":"policy_denied","message":"…"}}`.

## Layout

| path | contents |
| --- | --- |
| `~/.config/email/` (`$EMAIL_CONFIG_DIR`) | `config.toml` (`default_account`), `policy.toml`, `accounts/<alias>/` (0700; tokens and `imap-credentials.json` 0600) |
| `~/.local/state/email/` (`$EMAIL_STATE_DIR`) | `audit.jsonl`, `sync/<alias>-<label>.cursor`, `watch/<alias>.json` (watch's own cursor and notified ids) |

Account selection order: `-a`, then `$EMAIL_ACCOUNT`, then `default_account`, then the only configured account.

## Installing

`email` is one static binary for Linux and macOS (amd64, arm64).

```sh
# Script: picks your platform, checks checksums.txt, puts email in ~/.local/bin
# (/usr/local/bin as root). Rerunning with the same version changes nothing.
curl -fsSL https://github.com/james-tronai/email-cli/releases/latest/download/install.sh | sh
curl -fsSL https://github.com/james-tronai/email-cli/releases/latest/download/install.sh | sh -s -- --version v0.7.0 --dest /usr/local/bin

# Homebrew (macOS, Linux)
brew install james-tronai/tap/email

# Container (linux/amd64, linux/arm64; FROM scratch, entrypoint `email`)
docker run --rm -v email-data:/data ghcr.io/james-tronai/email-cli:v0.7.0 version
```

- **Options**: `install.sh --help` lists them. `--verify` also checks the release's keyless cosign signature, and needs `cosign`. No GitHub token is needed.
- **Kubernetes initContainer**: the image has no shell or `cp`. Use `email self-install --dest /opt/data/.local/bin` to copy the binary onto a shared volume.
- **Updates**: `email version --check` says whether a newer release exists. It never downloads anything.
- **Release assets**: `email_<os>_<arch>` (bare binaries), `checksums.txt`, `checksums.txt.sigstore.json`, one SBOM per binary, and `install.sh`. How releases are cut and verified: [docs/releasing.md](docs/releasing.md).

## Development

```sh
task check      # gofmt, vet, race tests
task build
```

The design doc is `docs/email-cli-design.md` in the k3s-infra repo (branch `james-tronai/link-email-hermes-james`).

## License

Apache License 2.0; see [LICENSE](LICENSE). Copyright 2026 James Tan.
