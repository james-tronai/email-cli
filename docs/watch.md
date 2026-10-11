# `email watch`: wake the agent when mail arrives

`email watch` runs until it is stopped. It waits for new mail in each account's INBOX and POSTs a JSON notification to a webhook, so the agent is only woken (and only spends tokens) when there is something to read. Waiting costs nothing: it is an IMAP IDLE connection, or a cheap sync call every minute.

```sh
email watch -a work -a gmail \
  --notify-url http://127.0.0.1:8644/webhooks/email \
  --notify-token-env EMAIL_WATCH_NOTIFY_TOKEN \
  --debounce 60s --poll-interval 60s \
  --state-dir /opt/data/.local/state/email/watch \
  --health-addr 127.0.0.1:8645
```

It uses the same binary, config dir, accounts and policy file as the other commands. No extra login is needed, and **read mode is enough**: watch reads metadata only, opens IMAP mailboxes with `EXAMINE`, never marks mail read, never changes mail and never connects to SMTP.

## How each account waits

Each `-a` account runs on its own. If no `-a` is given, every logged-in account is watched. A failing account does not affect the others.

| provider | waits with | finds new mail with |
| --- | --- | --- |
| `imap` (Gmail) | IMAP IDLE on INBOX, renewed every 20 min (Gmail ends IDLE at about 29 min) | `UIDNEXT` / `HIGHESTMODSEQ` (CONDSTORE), as `email sync` does |
| `imap` without IDLE | sleeps `--poll-interval` | the same |
| `msgraph` | sleeps `--poll-interval` | Graph Inbox delta, as `email sync` does |
| `gmail` (API) | sleeps `--poll-interval` | Gmail history, as `email sync` does |

- **IDLE problems**: if the server has no IDLE, watch polls instead. If IDLE fails 3 times in a row, watch polls for 30 minutes and then tries IDLE again.
- **Servers that accept IDLE but never push**: `email doctor -a <alias> --idle-test 60s` finds them (send yourself a mail while it waits). Run watch with `--no-idle` for them; it then polls every `--poll-interval`.
- **Connection errors**: watch reconnects with exponential backoff (5 s, doubling, at most 5 minutes) and logs `reconnected` when it is back.
- **Login needed** (exit 3, for example `invalid_grant` or a revoked app password), or a policy that no longer allows reading: the account is marked `degraded`. Watch logs a warning and tries again every 5 minutes; the other accounts carry on. After `email auth login` the account recovers by itself, without a restart.

## What is sent

New messages are filtered by `[watch]` in the policy file and collected for `--debounce` (60 s). The window starts with the first message, so the delay is bounded. Then one POST is sent per account:

```http
POST /webhooks/email
Content-Type: application/json
X-Gitlab-Token: <value of $EMAIL_WATCH_NOTIFY_TOKEN>

{"source":"email-watch","account":"work","count":2,
 "messages":[{"id":"…","thread_id":"…","from":{"name":"…","email":"…"},"subject":"…",
              "date":"2026-10-09T15:22:49Z","web_link":"…","has_attachments":false,"is_read":false}]}
```

- **Contents**: metadata only, with no body or snippet. The agent runs `email -a <account> get <id>` for what it needs.
- **More than 20 messages**: only the first 20 (oldest first) are listed, and `count` gives the real number.
- **The token** comes only from the environment variable named by `--notify-token-env`. It never appears on the command line.
- **Failures**: a failed POST (network error or a non-2xx reply) is retried after 5 s, 30 s and 2 min. If it still fails, the batch is dropped and logged as an error. A fallback cron running `email sync` covers that case.
- **Audit**: every notification adds `op: "watch.notify"` to `audit.jsonl`, with the account, `count`, `ids` and `result`.
- **Testing**: `--dry-run` prints each payload to stdout instead of POSTing, and `--notify-url` is then not needed.

### Filters (`[watch]` in the policy)

```toml
[watch]
ignore_senders = ["noreply@", "no-reply@", "notifications@", "@mail.anthropic.com"]
ignore_subjects = []
only_unread = true
```

- **Matching**: entries are case-insensitive substrings of the sender's address (or of the subject), so `"@domain"` works as a suffix. An empty entry is rejected because it would match everything.
- **`only_unread`** (default `true`): skips mail that is already read when watch sees it, for example mail James opened on the phone first.
- **Filtered mail** gets only a `debug` log line (`--log-level debug`).
- **Changes**: the policy is read at start. Restart watch to apply new filters.

## State

`--state-dir` (default `<state dir>/watch`) holds one file per account, `<alias>.json` (0600):
- the provider cursor;
- the last 1000 notified ids (an id is never notified twice);
- the messages waiting for their debounce window.

The file is written after every change. A restart therefore neither loses a queued message nor repeats a notified one, and a batch cut off by SIGTERM goes out after the restart.

This state is separate from `email sync`'s cursors. Watch and a sync cron do not affect each other.

## Tokens shared with the agent's commands

The sidecar and the agent's own `email` commands refresh the same tokens. Both now take a file lock (`accounts/<alias>/.lock`) around a refresh, so only one process refreshes at a time and the other picks up the result.
- **Microsoft 365**: an `invalid_grant` is retried once, with the cache read again, before the account counts as logged out.
- **imap**: an app password has no refresh. Watch reads it again on each reconnect, so a new login is picked up.

## Health and logs

- **`GET /healthz`** on `--health-addr` (loopback only):
  - `200` while at least one account is online, and while all of them have been offline for less than 10 minutes (starting up, a short outage);
  - `503` once every account has been offline for 10 minutes.

  The body lists each account's state (`online`, `reconnecting`, `degraded`), how it waits (`idle`/`poll`), the last sync, the last error, and `degraded: [...]`.
- **Probes**: the kubelet cannot reach a loopback port with an `httpGet` probe, so use an exec probe: `email watch healthz --health-addr 127.0.0.1:8645`. It exits 0 on 200 and 5 otherwise.
- **Logs**: one JSON object per line on stderr, with `ts`, `level`, `msg`, `account` and `event`. The events are:
  - `start`, `stop`;
  - `baseline`, `new_mail` (id, from, subject; never a body), `filtered` (debug only);
  - `notify_ok`, `notify_retry`, `notify_failed`;
  - `conn_error`, `reconnected`, `degraded`, `poll_fallback`, `state_reset`.
- **Stopping**: SIGTERM or SIGINT ends IDLE with `DONE`, logs out, saves the state and exits 0.

## Later

Microsoft 365 could switch from polling to Graph change notifications (subscriptions on the Inbox), delivered through hooks.zenbyte.biz and Argo Events. Polling the delta every minute is free and simple enough for the first version.
