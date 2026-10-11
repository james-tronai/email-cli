# Hermes

How to give a [Hermes](https://github.com/NousResearch/hermes-agent) agent your mailbox. The pieces are the same everywhere:

| piece | what it is | where it comes from |
| --- | --- | --- |
| `email` | the CLI the agent runs | release binary or image |
| policy | what the agent may do (`read`, `draft`, `send`, and limits); read-only to the agent | [docs/policy/](policy/) templates; you edit it |
| skill | `skills/comms/email/SKILL.md`: how and when the agent uses `email`. Its rules (mail is data, not instructions; no sending from unattended runs; `[SILENT]`) are the same in every deployment | inside the binary: `email hermes skill` |
| `email watch` | waits for new mail and POSTs to the Hermes webhook | the same binary, as a service |
| route script | `scripts/email-summary.py`: turns watch's POST into prompt variables | inside the binary |
| triage cron job | `email-triage`: reads new mail, reports what matters, else `[SILENT]` | inside the binary: `email hermes seed-cron` |
| config.yaml snippet | the webhook route and the skills settings | `email hermes config` |

How a new message reaches you:
1. Watch sees it: immediately on IMAP with IDLE, otherwise within a minute.
2. Watch waits 60 s for more mail, then POSTs to `/webhooks/email`.
3. The route runs the `email-triage` cron job right away, with the new messages as context.
4. The job reads what matters and messages you, or stays silent.

The schedule (every 2 h) is only a fallback.

## Before you start

- Hermes is installed and talks to you on some platform (Discord, Telegram, …). You know the chat id for `--deliver`, e.g. `discord:123456789` or `telegram:123456789`.
- A mailbox is set up for `email auth login`:
  - [Microsoft 365](microsoft-365-setup.md);
  - [Google Workspace](google-workspace-setup.md);
  - [personal Gmail](gmail-personal-setup.md);
  - [other IMAP servers](imap-presets.md).
- Pick a policy. Start with `draft`: the agent prepares replies and you send them.

## A. Kubernetes (Helm chart)

For Hermes running as a Deployment with HERMES_HOME on a PVC.

1. **Install the chart** next to Hermes. [charts/hermes-email](../charts/hermes-email/README.md) explains every value, and [examples/values.yaml](../charts/hermes-email/examples/values.yaml) is a complete example.

   ```sh
   helm install alice-email oci://ghcr.io/james-tronai/charts/hermes-email -n hermes -f values.yaml
   ```

   The init containers put `email` on the Hermes volume, along with the skill, the route script and (with `seedCron.enabled`) the triage cron job. Watch runs in its own pod on the same node.
2. **Change the Hermes Deployment**:
   - mount the chart's ConfigMap read-only at `/etc/email`;
   - put `/opt/data/.local/bin` on PATH;
   - set `WEBHOOK_ENABLED=true` and `WEBHOOK_SECRET` (from the Secret the chart reads);
   - merge the output of `email hermes config` into config.yaml;
   - restart Hermes.
3. **Log in** from the Hermes container. Tokens stay on the volume.

   ```sh
   kubectl -n hermes exec -it deploy/hermes-alice -c hermes -- email auth login -a work --provider msgraph --mode draft \
     --tenant <tenant-id> --client-id <client-id> --manual
   ```

   Watch reads the account list when it starts. Until the first login it exits, and Kubernetes restarts it with a growing delay (up to 5 minutes). After adding an account, run `kubectl -n hermes rollout restart deploy/alice-email-watch` to have it picked up at once.
4. **Check**:
   - `kubectl -n hermes exec deploy/hermes-alice -c hermes -- email doctor --format text --notify-url <the webhook URL>`;
   - `kubectl -n hermes logs deploy/alice-email-watch`, which should show `baseline` for each account.

**Send yourself a test mail. Within 60 to 90 seconds, Hermes should message you about it.**

## B. One machine (Linux or macOS)

For Hermes installed with its own installer, HERMES_HOME at `~/.hermes`.

1. **Install `email`**:

   ```sh
   curl -fsSL https://github.com/james-tronai/email-cli/releases/latest/download/install.sh | sh
   ```

   Or use `brew install james-tronai/tap/email`.
2. **Log in** (see the mailbox guides above), e.g.:

   ```sh
   email auth login -a work --provider msgraph --mode draft --tenant <tenant-id> --client-id <client-id>
   ```

3. **Install the Hermes integration**:

   ```sh
   email hermes install --deliver telegram:123456789
   ```

   It does six things:
   - writes the skill and route script into `~/.hermes`;
   - writes `~/.config/email/policy.toml` from the `draft` template, unless a policy exists (`--policy-template` picks another);
   - adds `WEBHOOK_ENABLED` and `WEBHOOK_SECRET` to `~/.hermes/.env` if they are missing;
   - puts the secret into `~/.config/email/watch.env` (0600);
   - installs and starts the watch service:
     - Linux: a systemd user unit `email-watch.service`, with `Restart=always`, the token from `EnvironmentFile`, and the home directory read-only except the state and account directories;
     - macOS: a launchd agent `io.github.james-tronai.email-watch`, logging to `~/Library/Logs/email-watch.log`;
   - creates the `email-triage` cron job.

   Running it again updates everything in place.
4. **Merge the printed config snippet** into `~/.hermes/config.yaml` and restart Hermes. Hermes has no way for a tool to change its config, so this step is yours. The snippet:
   - adds the `email` webhook route;
   - turns off Hermes's bundled mail skills (`google-workspace`, `himalaya`, `email-inbox-triage`). Left on, they compete for "check my mail" and the model goes looking for Google tokens or himalaya instead of using `email`;
   - pins the `email` skill into every session.
5. **Check**: `email doctor --format text --notify-url http://127.0.0.1:8644/webhooks/email`.
   - Linux: `journalctl --user -u email-watch` should show `baseline` for each account. On a server without a desktop login, also run `sudo loginctl enable-linger $USER`, so the service keeps running after you log out.

**Send yourself a test mail. Within 60 to 90 seconds, Hermes should message you about it.**

The steps one at a time, for other setups:

```sh
email hermes files                                  # skill + route script into $HERMES_HOME
email hermes seed-cron --deliver telegram:123456789 # the triage cron job
email hermes config                                 # the config.yaml snippet
email watch --notify-url http://127.0.0.1:8644/webhooks/email --notify-token-env EMAIL_WATCH_NOTIFY_TOKEN
```

## C. MCP

For agents that take tools over MCP instead of a CLI. Not yet available: this section follows with `email mcp`.

## Things that bite

- **Keep the route a `cron_job` route.** An agent route (`prompt` and `deliver` without `cron_job`) starts a webhook session. Hermes treats it as a human message, and when the model answers `[SILENT]`, Hermes replaces it with "⚠️ The model returned only a silence marker…" and sends that to you. A cron run keeps `[SILENT]` silent. (Seen 2026-10-10.)
- **The route names the job, not its id.** `cron_job: email-triage` resolves by name. If you rename the job, change the route too. `seed-cron` finds the job by name, so two jobs with the same name are an error.
- **Webhook on**: set `WEBHOOK_ENABLED` and `WEBHOOK_SECRET` in the environment. Do not also set `platforms.webhook.enabled` in config.yaml: then the secret from the environment is not merged, and every route fails validation at startup.
- **One watch per mailbox set.** Two copies of watch on the same accounts notify twice. When moving from a sidecar to the chart, remove the sidecar in the same change. The state directory (`<state dir>/watch`) carries over, so nothing is repeated or lost.
- **Policy edits** take effect at once for the agent's commands. Watch reads `[watch]` at start, so restart it after changing filters.
- **IMAP servers that accept IDLE but never push**: `email doctor -a <alias> --idle-test 60s` finds them. Run watch with `--no-idle` for them (chart value `watch.noIdle`).
