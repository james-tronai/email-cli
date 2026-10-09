# email

A multi-account mail CLI for self-hosted agents. The agent gets least-privilege, audited access and works in draft-only mode by default. Output is JSON, and the CLI never prompts.

Status: **M7**. Three providers, all with read, draft, organise, sync and send:

| provider | mailbox | login | setup |
| --- | --- | --- | --- |
| `msgraph` | Microsoft 365 (Graph) | browser, OAuth auth code + PKCE (`--manual` for headless) | [docs/microsoft-365-setup.md](docs/microsoft-365-setup.md) |
| `gmail` | Google Workspace (Gmail API) | the same flow, with a GCP Desktop OAuth client | [docs/google-workspace-setup.md](docs/google-workspace-setup.md) |
| `imap` | personal Gmail, or any IMAP/SMTP server | address + app password on stdin (`--password-stdin`) | [docs/gmail-personal-setup.md](docs/gmail-personal-setup.md) |

All three support list, search, get, thread, attachments, drafts (create/reply/forward/update/delete), move, label, mark, sync and send. `msgraph` has categories for labels and Outlook search syntax (KQL). `gmail` and `imap` on Gmail use Gmail labels and Gmail search syntax. `imap` on another server falls back to folders and plain IMAP `SEARCH`.

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

## Releases and installing

`email` is a single static binary, published only as GitHub Release assets. There is **no Homebrew formula, package or container image**: download the binary for your platform, check it against `checksums.txt`, and put it on `PATH`.

Each release is a GitHub Release on a `vX.Y.Z` tag of this (private) repo, with these assets:

| asset | contents |
| --- | --- |
| `email_linux_amd64` | static binary (`CGO_ENABLED=0`) |
| `email_linux_arm64` | static binary |
| `email_darwin_arm64` | static binary |
| `email_darwin_amd64` | static binary (from v0.4.0) |
| `checksums.txt` | `sha256sum` format: `<hex>  <asset name>` |

- Asset names have no version in them. The version lives in the tag only, and `email version` prints it.
- Names and the checksum format are a contract with the k3s initContainer. Change them only together with that side.
- Agent usage notes ship in [`skills/email/SKILL.md`](skills/email/SKILL.md). Deployments may carry their own adapted copy.

### Cutting a release

1. Write `.github/release-notes/vX.Y.Z.md`. Say what changed and, in particular, **what a deployment has to change**: policy keys, login flow, scopes, egress.
2. Commit it, then push the tag: `git tag -a vX.Y.Z -m … && git push origin vX.Y.Z`.
3. `.github/workflows/release.yml` runs the tests and [goreleaser](.goreleaser.yaml). Goreleaser builds the assets above, writes `checksums.txt` and publishes the release with the notes file, using the workflow's `GITHUB_TOKEN`.

`task snapshot` builds the same assets locally into `dist/` without publishing.

### Downloading (initContainer, or any script)

The repo is private, so unauthenticated requests get 404. Use a fine-grained PAT limited to this repo with **Contents: read**.

1. Look up the release by tag:
   `GET https://api.github.com/repos/james-tronai/email-cli/releases/tags/<tag>` (with `Authorization: Bearer <token>`).
2. From `.assets[]`, take the `url` of the asset by `name` (an `https://api.github.com/repos/…/releases/assets/<id>` URL). Use `url`, not `browser_download_url`, which does not work with a token for private repos.
3. `GET` that URL with `Authorization: Bearer <token>` and `Accept: application/octet-stream`. GitHub answers `302` with a signed, short-lived `release-assets.githubusercontent.com` URL.
4. Follow the redirect **without** the `Authorization` header. The signed URL carries its own credentials, and the storage host rejects or ignores a bearer token. `curl -L` already drops `Authorization` on a cross-host redirect; don't add `--location-trusted`.
5. Download `checksums.txt` the same way and verify before installing.

```sh
TAG=v0.1.0 ASSET=email_linux_amd64 REPO=james-tronai/email-cli
api() { curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" -H "X-GitHub-Api-Version: 2022-11-28" "$@"; }
rel=$(api "https://api.github.com/repos/$REPO/releases/tags/$TAG")
for name in "$ASSET" checksums.txt; do
  url=$(printf '%s' "$rel" | jq -r --arg n "$name" '.assets[] | select(.name == $n) | .url')
  api -H "Accept: application/octet-stream" -o "$name" "$url"
done
grep " $ASSET\$" checksums.txt | sha256sum -c -
install -m 0755 "$ASSET" /opt/data/.local/bin/email
```

## Development

```sh
task check      # gofmt, vet, race tests
task build
```

The design doc is `docs/email-cli-design.md` in the k3s-infra repo (branch `james-tronai/link-email-hermes-james`).
