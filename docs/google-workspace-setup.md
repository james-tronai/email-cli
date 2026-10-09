# Google Workspace setup

`email` uses the Gmail REST API with an OAuth client of type **Desktop app**. It acts as one signed-in user.

Login uses OAuth 2.0 **authorization code + PKCE** with a `http://localhost:<port>/callback` redirect: the same flow, and the same `--manual` paste-back for headless machines, as Microsoft 365. Google's device code flow does not support Gmail scopes, so there is no `--flow device` for Gmail.

This guide is for **Google Workspace** accounts. Personal `@gmail.com` accounts can only use an "External" app; an unpublished External app expires refresh tokens after 7 days, and publishing one with Gmail scopes requires Google's restricted-scope verification. A personal Gmail backend (IMAP with an app password) is planned for a later milestone.

Setup has two parts: create the OAuth client once per Workspace, then log in once per mailbox.

## 1. Create the OAuth client (once per Workspace)

Sign in to the Google Cloud console **with an account in the Workspace** (for example `james@tronai.com.au`), not a personal Google account. The "Internal" audience in step 4 only exists for projects that belong to the Workspace's organization.

1. Open [console.cloud.google.com](https://console.cloud.google.com) → project picker → **New project**.
   - **Name**: `email-cli`.
   - **Organization / Location**: the Workspace's organization (for example `tronai.com.au`), not "No organization".
2. With the new project selected: **APIs & Services** → **Library** → search **Gmail API** → **Enable**.
3. **Google Auth Platform** (formerly "OAuth consent screen") → **Get started**:
   - **App name**: `email-cli` (users see it on the consent screen).
   - **User support email**: your address.
4. **Audience**: **Internal**. Only accounts in the Workspace can sign in, nothing needs Google verification, and refresh tokens do not expire after 7 days.
5. **Contact information**: your address. Finish the wizard.
6. **Data access** → **Add or remove scopes**. Add:
   - `https://www.googleapis.com/auth/gmail.readonly` (mode `read`)
   - `https://www.googleapis.com/auth/gmail.modify` (modes `draft` and `send`)

   For an Internal app this list is informational, but it keeps the consent screen and the project in sync.
7. **Clients** → **Create client**:
   - **Application type**: **Desktop app**. A "Web application" client rejects the random loopback port with `redirect_uri_mismatch`.
   - **Name**: `email-cli (hermes)`.
   - **Create**, then **Download JSON**. The file is named `client_secret_<id>.apps.googleusercontent.com.json`.

### About the client secret

Google treats the secret of a Desktop client as **not confidential**: it is shipped inside installed apps, and PKCE is what protects the login. Even so:

- **Never commit the JSON file or the secret to Git.**
- `email` stores the client in `accounts/<alias>/google-client.json` (mode 0600), never in `account.json`, and `auth status` does not print it.
- After the first login, a re-login of the same alias needs neither `--client-file` nor the secret.

### Workspace admin settings

Internal apps are usually allowed without further steps. If login fails with `admin_policy_enforced` or "access blocked by your administrator":

1. Open [admin.google.com](https://admin.google.com) → **Security** → **Access and data control** → **API controls**.
2. Under **Manage Third-Party App Access**, add the client ID and set it to **Trusted**.

## 2. Log in a mailbox (machine with a browser)

```sh
email auth login -a gw --provider gmail --mode draft \
  --client-file ~/Downloads/client_secret_XXXX.apps.googleusercontent.com.json
```

`email` starts a listener on `http://localhost:<random port>/callback` and opens the browser:

1. Pick the Workspace account.
2. On the consent screen, **leave the Gmail permission ticked**. Google lets users untick individual scopes. If the Gmail scope is unticked, login fails with "the consent screen did not grant …".
3. When the browser shows "Signed in", the command prints the account as JSON.

You can pass `--client-id <id> --client-secret <secret>` instead of `--client-file`.

The alias is locked to the address from the Gmail profile (`users/me/profile`). Logging the alias in again as a different mailbox is refused; use another alias, or `email auth logout -a gw` first.

| `--mode` | scope requested | what the token can do |
| --- | --- | --- |
| `read` | `gmail.readonly` | read only |
| `draft` | `gmail.modify` | read, label, drafts, **and send** |
| `send` | `gmail.modify` | same as draft |

Gmail has no scope that creates drafts without also being able to send: both `gmail.compose` and `gmail.modify` can send. On Gmail, a draft-mode account is kept from sending **by the policy file only** (`[default] mode = "draft"` makes `email send` and `email api POST …/send` exit 4). Login prints a warning to that effect. On Microsoft 365, a draft-mode token has no `Mail.Send` at all.

## 3. Headless login (`--manual`, e.g. kubectl exec)

There is no browser in a Pod. `--manual` prints the authorize URL and reads the redirect URL back on stdin:

```sh
kubectl -n hermes exec -i deploy/hermes-james -c hermes -- \
  email auth login -a gw --provider gmail --mode draft --manual \
  --client-id <id>.apps.googleusercontent.com --client-secret <secret>
```

`-i` is required (stdin), and so is `-c hermes` (the Pod has several containers).

1. The command prints `{"mode":"manual","authorize_url":"https://accounts.google.com/o/oauth2/v2/auth?…","redirect_uri":"http://localhost:5xxxx/callback",…}` and waits.
2. Open `authorize_url` in any browser and sign in as the Workspace user.
3. Google redirects to `http://localhost:5xxxx/callback?state=…&code=…&scope=…`. Nothing listens there, so the browser shows a connection error. That is expected.
4. Copy the **whole** address-bar URL, paste it into the terminal and press Enter. The code is single-use and expires within minutes.

To use `--client-file` in a Pod instead, copy the file in, log in, then delete it:

```sh
kubectl -n hermes cp client_secret.json hermes-james-xxxx:/tmp/client_secret.json -c hermes
kubectl -n hermes exec -i deploy/hermes-james -c hermes -- \
  email auth login -a gw --provider gmail --mode draft --manual --client-file /tmp/client_secret.json
kubectl -n hermes exec deploy/hermes-james -c hermes -- rm /tmp/client_secret.json
```

The Pod needs outbound HTTPS to `oauth2.googleapis.com` (token refresh) and `gmail.googleapis.com` (API). `accounts.google.com` is only opened in the browser.

## Several accounts

The first account logged in becomes `default_account`. A Gmail account added next to an existing Microsoft 365 account is therefore **not** the default: pass `-a gw`, or set `EMAIL_ACCOUNT=gw`. `email accounts list` shows which one is the default.

## Staying logged in

- Every command refreshes the access token when it has expired, and writes the new token back to `accounts/<alias>/google-token.json` (0600). After a refresh, the mailbox is checked against the account lock again.
- Internal apps' refresh tokens do not expire on a schedule. They are revoked when the user removes the app at [myaccount.google.com/permissions](https://myaccount.google.com/permissions), when the password changes (with Gmail scopes), when an admin removes access, or after 6 months without use.
- When the refresh token stops working, every command exits **3** with `token refresh failed (invalid_grant …)`. Run the login again.

## Ids and links

- Message and thread ids are Gmail's hex ids. Draft commands return the **draft id** (`r-…`), which `draft update`, `draft delete`, `draft get` and `send` take. Passing a draft's message id (as `list --label drafts` shows it) also works.
- `web_link` on a message is `https://mail.google.com/mail/u/0/#inbox/<id>`. On a draft it is `https://mail.google.com/mail/u/0/#drafts?compose=<message id>`, which opens the draft in the compose window. `/u/0/` is the first Google account signed in to that browser.
- Attachment ids are MIME part ids (`1`, `2.1`). They are stable, unlike Gmail's own attachment ids, which change on every fetch.
- `search` passes the query to Gmail unchanged, so the syntax is the Gmail search box's: `from:alice has:attachment newer_than:7d`.
- `move --to archive` removes `INBOX`; `move --to <label>` adds the label and removes `INBOX` (Gmail's "Move to"). `label --add <name>` creates a user label that does not exist yet.
- `sync` uses `history.list`. If the stored history id is too old (Gmail keeps about a week of history), the next sync records a new baseline and reports `initialized: true`.

## Troubleshooting

| error | cause | fix |
| --- | --- | --- |
| `Error 400: redirect_uri_mismatch` | the client is not a Desktop app | create a **Desktop app** client (step 7) |
| `Error 403: org_internal` | the account is outside the Workspace that owns the project | sign in with a Workspace account, or create the project in that Workspace |
| `access_denied` / `admin_policy_enforced` | the Workspace admin blocks the app | mark the client **Trusted** in API controls |
| `the consent screen did not grant …` | the Gmail checkbox was unticked | log in again and leave it ticked |
| `Google returned no refresh token` | an earlier grant was reused without a new refresh token | remove `email-cli` at myaccount.google.com/permissions and log in again |
| `state mismatch` | the pasted URL is from another login attempt | start the login again and paste the newest URL |
| `token refresh failed (invalid_grant …)` | access revoked, password changed, or the token unused for 6 months | log in again |
| `the token lacks the scope for this call` | the account logged in with `--mode read` | log in again with `--mode draft` |
| `Gmail API has not been used in project … or it is disabled` | step 2 was skipped | enable the Gmail API in the project |
