# email

Your mailbox for your AI agent: Microsoft 365, Gmail, Google Workspace and IMAP servers, behind one CLI and an MCP server. The agent reads mail, drafts replies and (only if you allow it) sends. It works under a policy it cannot change, every write is logged, and message bodies are handed over as untrusted data, never as instructions. New mail wakes the agent within about a minute.

Tokens and passwords stay on your machine. There is no hosted service.

## Pick your path

| you run | quickstart | the agent uses | new mail arrives by |
| --- | --- | --- | --- |
| [Hermes](https://github.com/NousResearch/hermes-agent) on Kubernetes | [A](#a-hermes-on-kubernetes) | the `email` CLI + skill | `email watch` in its own pod |
| Hermes on one machine (VPS, Mac, Raspberry Pi) | [B](#b-hermes-on-one-machine) | the `email` CLI + skill | `email watch` as a systemd / launchd service |
| Claude Code, Codex, or another MCP client | [C](#c-claude-code-codex-and-other-mcp-clients) | MCP tools | you ask; or watch → your webhook |

Each takes about 30 minutes, most of it connecting the mailbox. All three need a mailbox you can log in to:

| mailbox | provider | you need | guide |
| --- | --- | --- | --- |
| Microsoft 365 | `msgraph` | an app registration in your Entra tenant (10 min, admin) | [microsoft-365-setup.md](docs/microsoft-365-setup.md) |
| personal Gmail | `imap` | an app password (2-step verification on) | [gmail-personal-setup.md](docs/gmail-personal-setup.md) |
| Google Workspace | `gmail` | a GCP project with a Desktop OAuth client | [google-workspace-setup.md](docs/google-workspace-setup.md) |
| iCloud, Fastmail, QQ, 163, Alibaba Mail, Mailu, any IMAP/SMTP | `imap` | an app password | [imap-presets.md](docs/imap-presets.md) |

After each step, `email doctor --format text` checks the install, the policy, each login and the watch setup, and says how to fix what fails.

## Try it first (no mailbox, 2 minutes)

```sh
curl -fsSL https://github.com/james-tronai/email-cli/releases/latest/download/install.sh | sh
export EMAIL_CONFIG_DIR=/tmp/email-try/config EMAIL_STATE_DIR=/tmp/email-try/state
email auth login -a demo --provider fake --mode draft -q      # a built-in pretend mailbox
curl -fsSL https://raw.githubusercontent.com/james-tronai/email-cli/main/docs/policy/draft.toml \
  -o /tmp/email-try/config/policy.toml                          # without a policy, everything is read-only
email list --format text
email get fake-0002 --format text                # hidden text in the HTML is removed and flagged
email draft reply fake-0002 --body "Thanks"
email send fake-0004                             # exit 4: the draft policy does not send
```

## A. Hermes on Kubernetes

For Hermes running as a Deployment with HERMES_HOME on a PVC. The chart adds `email`, the skill, the triage cron job and a watch pod next to your Deployment; it does not change the Deployment.

1. **Install the chart**, starting from [examples/values.yaml](charts/hermes-email/examples/values.yaml):

   ```sh
   helm install alice-email oci://ghcr.io/james-tronai/charts/hermes-email -n hermes -f values.yaml
   ```

2. **Change the Hermes Deployment**: mount the chart's ConfigMap at `/etc/email`, put `/opt/data/.local/bin` on PATH, set `WEBHOOK_ENABLED=true` and `WEBHOOK_SECRET`, merge `email hermes config` into config.yaml, restart.
3. **Log in** from the Hermes container (the guides above have the values):

   ```sh
   kubectl -n hermes exec -it deploy/hermes-alice -c hermes -- \
     email auth login -a work --provider msgraph --mode draft --tenant <tenant-id> --client-id <client-id> --manual
   kubectl -n hermes rollout restart deploy/alice-email-watch
   ```

4. **Check**: `kubectl -n hermes exec deploy/hermes-alice -c hermes -- email doctor --format text`.

Details for each step: [docs/hermes.md § A](docs/hermes.md#a-kubernetes-helm-chart) and the [chart README](charts/hermes-email/README.md).

## B. Hermes on one machine

For Hermes installed with its own installer (HERMES_HOME at `~/.hermes`), on Linux or macOS.

1. **Install** `email` (or `brew install james-tronai/tap/email`):

   ```sh
   curl -fsSL https://github.com/james-tronai/email-cli/releases/latest/download/install.sh | sh
   ```

2. **Log in** to your mailbox (the guides above), for example:

   ```sh
   email auth login -a work --provider msgraph --mode draft --tenant <tenant-id> --client-id <client-id>
   printf '%s' "$APP_PASSWORD" | email auth login -a gmail --provider imap --username you@gmail.com --mode draft --password-stdin
   ```

3. **Connect Hermes**: the skill, a `draft` policy, the watch service and the triage cron job reporting to you:

   ```sh
   email hermes install --deliver telegram:123456789 --format text   # or discord:<channel id>, …
   ```

4. **Merge the printed snippet** into `~/.hermes/config.yaml` and restart Hermes.
5. **Check**: `email doctor --format text --notify-url http://127.0.0.1:8644/webhooks/email`.

Details: [docs/hermes.md § B](docs/hermes.md#b-one-machine-linux-or-macos).

**A and B: send a test mail to the mailbox from another address, and leave it unread. Within 60 to 90 seconds Hermes messages you about it. Ask it to draft a reply; it gives you the draft and a link, and you send it.**

## C. Claude Code, Codex and other MCP clients

1. **Install and log in** as in B, steps 1 and 2.
2. **Add the MCP server**:

   ```sh
   claude mcp add email -- email mcp                      # Claude Code
   codex mcp add email -- email mcp                       # Codex
   ```

   Any other client: command `email`, arguments `mcp` (stdio). For an agent on another machine, `email mcp --http` serves Streamable HTTP with a bearer token.
3. **Ask** the agent to "check my mail", then to "draft a reply to the newest message". It uses the `email` tools, shows you the draft and its link, and sends nothing unless you ask.

Details, HTTP setup and the tool list: [docs/mcp.md](docs/mcp.md). An agent with a shell can also run `email` directly; `email hermes skill` prints the instructions Hermes uses, which suit any agent. To be told about new mail, run [`email watch`](docs/watch.md) against your own webhook.

## What the agent can do

You set this in a policy file, `~/.config/email/policy.toml`. Start from a template in [docs/policy/](docs/policy/):

| template | the agent may |
| --- | --- |
| `read` | read and search |
| `draft` (recommended) | also write drafts, mark, move and label; never send |
| `send-reply-only` | also send replies in existing threads, to at most 5 recipients |
| `send-open` | also send new messages, to at most 10 recipients |

Sending also needs the account logged in with `--mode send`, and the skill and the MCP server tell the agent to send only a draft you have seen and asked it to send. Trash and delete stay off unless you turn them on.

The policy binds the agent only where the agent cannot edit it: the Helm chart mounts it read-only. On one machine Hermes's terminal runs as you, so there the policy stops mistakes and injected instructions acting through `email`, not an agent determined to get around it (the skill tells it never to touch the file). The hard limit there is the login: a Microsoft 365 account logged in with `--mode draft` cannot send at all. Every write and every denial goes to `~/.local/state/email/audit.jsonl`. More: [docs/safety.md](docs/safety.md).

## Documentation

- [install.md](docs/install.md): all install methods, files, uninstalling
- [safety.md](docs/safety.md): modes, policy, untrusted content, sending
- [reference.md](docs/reference.md): output, exit codes, accounts, environment, compatibility
- [hermes.md](docs/hermes.md): Hermes in detail, and the things that bite
- [mcp.md](docs/mcp.md): the MCP server
- [watch.md](docs/watch.md): new-mail notifications
- mailbox setup: [Microsoft 365](docs/microsoft-365-setup.md), [Google Workspace](docs/google-workspace-setup.md), [personal Gmail](docs/gmail-personal-setup.md), [IMAP presets](docs/imap-presets.md)
- [releasing.md](docs/releasing.md): how releases are cut and verified

## Development

```sh
task check      # gofmt, vet, race tests
task build
```

## License

Apache License 2.0; see [LICENSE](LICENSE). Copyright 2026 James Tan.
