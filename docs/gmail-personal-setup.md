# Personal Gmail setup (IMAP + app password)

For a personal `@gmail.com` account, `email` uses Gmail's IMAP and SMTP servers with an **app password**: provider `imap`. The Gmail API route ([google-workspace-setup.md](google-workspace-setup.md)) only works for Google Workspace accounts.

| | |
| --- | --- |
| IMAP | `imap.gmail.com:993`, implicit TLS (the default for a `gmail.com` address) |
| SMTP | `smtp.gmail.com:465`, implicit TLS; used only by `email send` |
| Login | the Gmail address plus a 16-letter app password, read from stdin |
| Stored as | `accounts/<alias>/imap-credentials.json` (0600). `account.json` (what `auth status` prints) holds only the hosts. |

**What an app password can do.** An app password can do everything with the mailbox: read, change, delete and send. Google offers no read-only or draft-only app password. So for an `imap` account, `--mode read|draft|send` is kept by the policy file and by this client:
- below `draft` the client refuses every change;
- below `send` it never opens an SMTP connection.

Keep the Pod's policy at `mode = "draft"`. Login prints a warning saying so.

## 1. Turn on 2-Step Verification

App passwords only exist on accounts with 2-Step Verification.

1. Sign in to [myaccount.google.com](https://myaccount.google.com) as the Gmail account.
2. **Security** → **How you sign in to Google** → **2-Step Verification** → turn it on (an authenticator app or a passkey; SMS also works).

If the account is enrolled in Google's **Advanced Protection**, app passwords are not available.

## 2. Create an app password

1. Open [myaccount.google.com/apppasswords](https://myaccount.google.com/apppasswords). The page is not linked from the menus; open the URL directly. It asks for the Google password again.
2. **App name**: something you will recognise later, for example `hermes-email-cli`.
3. **Create**. Google shows a 16-letter password in four groups, for example `abcd efgh ijkl mnop`. It is shown only once.

Do not paste it into a chat or a ticket. Keep it on the clipboard or in a password manager only until step 3 is done. The spaces are only how Google displays it. `email` removes them for Gmail, so either form works.

## 3. Log in (once per mailbox)

**Locally:**

```sh
email auth login -a gmail --provider imap --mode draft --username you@gmail.com --password-stdin
# paste the app password, press Enter, then Ctrl-D
```

**In the Pod.** Run this from your own terminal. The password goes from your keyboard through `kubectl exec -i` into the CLI's stdin, and never passes through the agent or a chat:

```sh
kubectl -n hermes exec -i deploy/hermes-james -c hermes -- \
  email auth login -a gmail --provider imap --mode draft --username <addr> --password-stdin
# paste the app password, press Enter, then Ctrl-D
```

Or from a password manager, without the clipboard (1Password CLI as an example):

```sh
op read 'op://Private/Gmail app password/password' | \
  kubectl -n hermes exec -i deploy/hermes-james -c hermes -- \
  email auth login -a gmail --provider imap --mode draft --username <addr> --password-stdin
```

Login connects once to `imap.gmail.com`, checks the password with an IMAP `LOGIN`, and stores it only if the check succeeds. The output is the account (no password) and the warnings.

The password is accepted only on stdin. There is no `--password` flag and no environment variable, because both would show up in shell history, `ps` output or the Pod spec.

**Account lock.** The alias is locked to the address it was first logged in with. Logging `gmail` in again as another address fails with exit 3. To switch, run `email auth logout -a gmail` first. Logging in again as the same address (for example with a new app password) keeps the alias.

**Several accounts.** The Pod's default account stays the one that was logged in first. Gmail commands need `-a gmail`, or `default_account = "gmail"` in `config.toml`.

## 4. Check it

```sh
email -a gmail auth status        # provider imap, servers, mode; no password
email -a gmail capabilities       # commands the policy allows
email -a gmail list --limit 5
```

## How Gmail looks through IMAP

The server announces Gmail's IMAP extensions (`X-GM-EXT-1`), and the client uses them:

| | |
| --- | --- |
| message id | `X-GM-MSGID` in hex: the same id the Gmail web client and API use |
| `web_link` | `https://mail.google.com/mail/u/0/#inbox/<id>`; a draft is `…#drafts/<id>` |
| thread id | `X-GM-THRID` in hex; `email thread` shows the whole conversation |
| labels | `X-GM-LABELS`. `inbox`, `sent`, `drafts`, `trash` and `spam` map to Gmail's system labels; `archive` means "in none of them" |
| `search` | the query goes to `X-GM-RAW`, so it uses Gmail search box syntax (`from:alice newer_than:7d has:attachment`) |
| `sync` | INBOX's `UIDVALIDITY` + `UIDNEXT`, plus `HIGHESTMODSEQ` (CONDSTORE) to skip unchanged runs. A changed `UIDVALIDITY` starts a new baseline (`initialized: true`). |
| drafts | `APPEND` to `[Gmail]/Drafts` with `\Draft`. `draft update` saves a new version and deletes the old one, **so the draft's id changes**: use the id it returns. |
| `send` | the draft goes out through `smtp.gmail.com`, then it is deleted from Drafts. Gmail puts the sent copy in Sent itself. |

Each command opens one IMAP connection and logs out when it finishes. Gmail allows 15 simultaneous IMAP connections per account, and the cron job runs every 30 minutes, so the client never keeps one open.

The same provider works with other IMAP servers: pass `--imap-host`, and `--smtp-host` if you will send. Without `X-GM-EXT-1`:
- ids are UIDs (`<uidvalidity>.<uid>.<mailbox>`);
- threads are rebuilt from `References` / `In-Reply-To`;
- folders replace labels (`email label` is unsupported; use `move`);
- search accepts `from: to: cc: subject: body: in:<folder> is:unread since:/before:YYYY-MM-DD`, plus free words.

## Revoking access

- **Delete the app password**: [myaccount.google.com/apppasswords](https://myaccount.google.com/apppasswords) → the trash icon next to it. Every login that uses it stops working at once: the next command exits 3. Nothing else on the account is affected.
- **Changing the Google password** also revokes every app password.
- **On the Pod**, remove the stored copy too: `kubectl -n hermes exec deploy/hermes-james -c hermes -- email auth logout -a gmail`.

To rotate, create a new app password, log in again with the same `-a gmail --username <addr>`, then delete the old one.
