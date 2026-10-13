# Reference

`email --help` and `email <command> --help` describe every command and flag. This page has what applies to all of them.

## Output

- **JSON by default**, on stdout. `--format text` is for people (bodies inside `UNTRUSTED MESSAGE BODY` fences); `--format id` prints only ids, one per line. `--jq <expr>` filters the JSON.
- **Never prompts.** Anything interactive (`auth login --manual`, `--password-stdin`) reads stdin, and only when you ask for it.
- **Logs and errors go to stderr.** An error is one line of JSON:

  ```json
  {"error":{"exit_code":4,"code":"policy_denied","message":"…"}}
  ```

  `code` is one of:

  | code | exit | |
  | --- | --- | --- |
  | `usage` | 2 | bad command, flag or argument |
  | `unsupported` | 2 | this provider cannot do it |
  | `auth_required` | 3 | no such account, or log in again |
  | `policy_denied` | 4 | the policy does not allow it |
  | `not_found` | 5 | the message, draft, thread or attachment does not exist (any more) |
  | `send_in_progress` | 5 | a send with the same idempotency key has not finished |
  | `provider_error` | 5 | anything else from the mail server or API |
  | `unreachable`, `unhealthy` | 5 | `email watch healthz`: watch is not running, or reports a problem |
  | `timeout` | 6 | `--timeout` ran out |
  | `internal` | 1 | a bug |

  Errors from the provider (including `not_found`) add `provider_code` with the upstream code.

## Exit codes

| code | meaning |
| --- | --- |
| 0 | ok |
| 1 | internal error (a bug; please report it). `email doctor` also exits 1 when a check fails |
| 2 | usage error, or the provider lacks the capability |
| 3 | authentication required (no account, account lock mismatch, expired token) |
| 4 | denied by policy |
| 5 | provider error (`provider_code` carries the upstream code) |
| 6 | timeout |

## Accounts

Each account has an alias (`-a work`). The account used is the first of:
1. `-a`;
2. `$EMAIL_ACCOUNT`;
3. `default_account` in `config.toml`;
4. the only configured account.

`email accounts list` shows them, with each login's mode and the effective mode under the policy.

## Environment

| variable | default | |
| --- | --- | --- |
| `EMAIL_CONFIG_DIR` | `~/.config/email` | accounts, `config.toml`, `policy.toml` |
| `EMAIL_STATE_DIR` | `~/.local/state/email` | audit log, sync cursors, send records, watch state |
| `EMAIL_POLICY_FILE` | `<config dir>/policy.toml` | the policy |
| `EMAIL_ACCOUNT` | | account alias |
| `EMAIL_WATCH_NOTIFY_TOKEN` | | `email watch`: the webhook secret (`--notify-token-env` names another) |
| `EMAIL_MCP_TOKEN` | | `email mcp --http`: the bearer token (`--token-env` names another) |

## Compatibility

From v1.0.0, versions follow [semantic versioning](https://semver.org). A change that breaks any of the following needs a new major version:

- **Release assets**: the names `email_<os>_<arch>`, `checksums.txt`, `checksums.txt.sigstore.json`, `install.sh`; the image `ghcr.io/james-tronai/email-cli` and its entrypoint; the chart `oci://ghcr.io/james-tronai/charts/hermes-email`.
- **Commands and flags**: existing ones keep working with the same meaning.
- **JSON output**: existing fields keep their name, type and meaning. This includes errors, the `email watch` webhook payload, and the MCP tool results.
- **Exit codes and error `code` strings.**
- **Policy keys**, and what a missing key means. Unknown keys stay an error, so a policy written for a newer minor version may be refused by an older binary.
- **Files**: the config and state layout above. A newer version reads what an older one wrote.
- **MCP**: tool names and argument names.
- **Chart values**: existing keys keep their meaning.

These may change in a minor or patch release:
- new commands, flags, JSON fields, error codes, policy keys, MCP tools and chart values (read JSON with a parser that ignores unknown fields);
- `--format text` output, log lines on stderr, and `--help` wording;
- the skill text, the triage prompt and the MCP server instructions (they are tuned as models change, within the same rules);
- `email api`, which passes provider responses through as they are.
