# email

A multi-account mail CLI for self-hosted agents. The agent gets least-privilege, audited access and works in draft-only mode by default. Output is JSON, and the CLI never prompts.

Status: **M2**. Microsoft 365 (Graph) read + draft works: browser login (auth code + PKCE, `--manual` for headless), list, search, get, thread, attachments, drafts (create/reply/forward/update/delete), move, categories, mark and delta sync. Setup steps are in [docs/microsoft-365-setup.md](docs/microsoft-365-setup.md). Gmail is M3; send is M4.

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

- **Modes** `read` < `draft` < `send`. The effective mode is the lower of the policy mode and the mode the account logged in with. Login only requests the scopes that mode needs, so a draft-mode M365 token has no `Mail.Send`. Gmail is different: no Gmail scope can create drafts without also being able to send (`gmail.modify` can send), so on Gmail only the CLI's policy gate blocks sending.
- **Policy file**: `$EMAIL_POLICY_FILE`, else `~/.config/email/policy.toml` (see [docs/policy.example.toml](docs/policy.example.toml)). A missing file means `read`. An unparsable file, or one with unknown keys, stops every command with exit 4.
- **Audit**: every write and every denial is appended to `~/.local/state/email/audit.jsonl`.
- **Untrusted content**:
  - Message bodies always go in `body_text`.
  - Text output wraps bodies in `BEGIN/END UNTRUSTED MESSAGE BODY` fences; a fence marker inside a body is defused.
  - Hidden HTML text (white-on-white, `display:none`, zero font size, …) is stripped and flagged with `body_hidden_text_removed`. When a message has HTML, the text is derived from that HTML rather than from the sender's `text/plain` part, so the agent reads what a human would see.
  - Bodies are truncated at 20 000 characters.
  - Attachments are only saved by an explicit `download`, with sanitized names and mode 0600.
- **Trash and delete** are disabled unless `[delete] enabled = true`. Draft deletion is always allowed.

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
| `~/.config/email/` (`$EMAIL_CONFIG_DIR`) | `config.toml` (`default_account`), `policy.toml`, `accounts/<alias>/` (0700, tokens 0600) |
| `~/.local/state/email/` (`$EMAIL_STATE_DIR`) | `audit.jsonl`, `sync/<alias>-<label>.cursor` |

Account selection order: `-a`, then `$EMAIL_ACCOUNT`, then `default_account`, then the only configured account.

## Development

```sh
task check      # gofmt, vet, race tests
task build
```

The design doc is `docs/email-cli-design.md` in the k3s-infra repo (branch `james-tronai/link-email-hermes-james`).
