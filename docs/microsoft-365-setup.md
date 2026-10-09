# Microsoft 365 setup

`email` uses Microsoft Graph with **delegated** permissions. It acts as one signed-in user and never holds a client secret.

Login uses OAuth 2.0 **authorization code + PKCE** with a `http://localhost:<port>/callback` redirect. The device code flow is still available as `--flow device`, but it is not the default: Entra **Security Defaults block device code sign-in** (`AADSTS530035`), and turning Security Defaults off is not an option in a tenant without Conditional Access.

Setup has two parts: register an app in Entra once per tenant, then log in once per mailbox.

## 1. Register the app in Entra (once per tenant)

You need an account that can register applications in the tenant (by default any member can; tenants that restrict this need an Application Developer or admin).

1. Open the [Entra admin center](https://entra.microsoft.com) → **Identity** → **Applications** → **App registrations** → **New registration**.
2. Fill in:
   - **Name**: `email-cli` (any name; users see it on the consent screen).
   - **Supported account types**: **Accounts in this organizational directory only** (single tenant).
   - **Redirect URI**: leave empty for now.
   - Click **Register**.
3. On the app's **Overview** page, copy:
   - **Application (client) ID** → `--client-id`
   - **Directory (tenant) ID** → `--tenant`
4. **Authentication** → **Add a platform** → **Mobile and desktop applications**. Add these custom redirect URIs, then click **Configure**:
   - `http://localhost/callback` (what `email` sends; for `localhost`, Entra accepts any port but compares the path)
   - `http://localhost`
5. Still on **Authentication**, under **Advanced settings**, set **Allow public client flows** to **Yes** and save. Without it, the token exchange fails with `AADSTS7000218` ("client_assertion or client_secret required").
6. **API permissions** → **Add a permission** → **Microsoft Graph** → **Delegated permissions**. Add:

   | Permission | Needed for |
   | --- | --- |
   | `User.Read` | identifying the mailbox at login (account lock) |
   | `Mail.Read` | `read` mode |
   | `Mail.ReadWrite` | `draft` mode (drafts, move, categories, read/flag state) |
   | `offline_access` | refresh tokens (stay logged in) |

   - **Do not add `Mail.Send`** while you run in draft mode. Entra puts every consented permission into the token, whatever the CLI asks for. If `Mail.Send` is consented, a draft-mode token can send mail outside the CLI. `email auth login` warns when this happens.
   - **Do not add any Application permissions.** They grant access to every mailbox in the tenant.
7. Optional: click **Grant admin consent for <tenant>**. Users then don't see a consent prompt, and the login works in tenants where user consent is disabled.

There is no client secret and no certificate. Keep **Certificates & secrets** empty.

### The tronai.com.au registration

The app for hermes-james is already registered. Neither ID is secret:

| | |
| --- | --- |
| App name | `email-cli` (hermes-james) |
| Directory (tenant) ID | `97acb66e-0e4a-4325-a2f0-76b5ea22b8d2` (tronai.com.au, single tenant) |
| Application (client) ID | `ad23d4f7-ab01-4825-93d6-ff2be487f96d` |
| Object ID | `3812590a-eb93-409f-ac60-2c9265fd0ab8` |
| Delegated permissions | `User.Read`, `Mail.Read`, `Mail.ReadWrite`, `offline_access` (admin consented; no `Mail.Send`) |

## 2. Log in a mailbox (machine with a browser)

```sh
email auth login -a work --provider msgraph --mode draft \
  --tenant 97acb66e-0e4a-4325-a2f0-76b5ea22b8d2 \
  --client-id ad23d4f7-ab01-4825-93d6-ff2be487f96d
```

- `-a work` is the local alias. Pass it later with `-a`, or rely on `default_account`. The first account becomes the default.
- `--mode` decides which scopes are requested: `read` = `User.Read Mail.Read offline_access`; `draft` adds `Mail.ReadWrite`; `send` adds `Mail.Send`. Moving to a higher mode means logging in again.
- `--tenant` defaults to `organizations` (any work or school account). `common` and `consumers` are refused: personal Microsoft accounts are not supported.
- `--tenant` and `--client-id` are saved in `account.json`, so a later re-login only needs `-a work --provider msgraph --mode …`.

The command listens on `http://localhost:<random port>/callback`, opens the browser (skip that with `--no-browser`) and prints:

```json
{
  "mode": "loopback",
  "authorize_url": "https://login.microsoftonline.com/97acb66e-…/oauth2/v2.0/authorize?client_id=…&code_challenge=…",
  "redirect_uri": "http://localhost:51234/callback",
  "instructions": "Sign in in the browser window that opened (or open authorize_url on this machine). Waiting for the redirect…"
}
```

Sign in as the mailbox owner, MFA included. The browser lands on a "Signed in" page and the command prints the result:

```json
{
  "account": "work",
  "provider": "msgraph",
  "mode": "draft",
  "username": "james@tronai.com.au",
  "scopes": ["User.Read", "Mail.Read", "offline_access", "Mail.ReadWrite"],
  "default": true
}
```

`username` comes from Graph `/me` (`userPrincipalName`) and **locks** the alias to that mailbox. Every later command checks that the cached token still belongs to that user and refuses (exit 3) if it does not. Logging in again under the same alias with a different user is also refused. Run `email auth logout -a work` first if you really mean to switch mailboxes.

Then check:

```sh
email auth status -a work
email capabilities -a work
email list -a work --limit 5 --format text
```

## 3. Headless login (`--manual`, e.g. kubectl exec)

When the CLI runs where no browser can reach its loopback port (a Pod, an SSH session), add `--manual`. The browser can be anywhere: the owner's laptop or phone.

```sh
kubectl -n hermes exec -i deploy/hermes-james -c hermes -- \
  email auth login -a work --provider msgraph --mode draft --manual \
    --tenant 97acb66e-0e4a-4325-a2f0-76b5ea22b8d2 \
    --client-id ad23d4f7-ab01-4825-93d6-ff2be487f96d
```

1. The command prints a JSON prompt with `"mode": "manual"`, an `authorize_url` and a `redirect_uri` (`http://localhost:<port>/callback`), then waits for one line on stdin. `-i` is required so stdin reaches it.
2. Send `authorize_url` to the mailbox owner (for example in a Discord DM). To print only the URL, add `--jq 'select(.authorize_url) | .authorize_url'`.
3. The owner opens the URL in any browser and signs in.
4. The browser is then redirected to `http://localhost:<port>/callback?code=…&state=…`. **Nothing listens there, so the page fails to load. That is expected.**
5. The owner copies the **entire URL from the address bar**. It contains the one-time code, valid for a few minutes.
6. Paste that URL into the waiting command and press Enter.

The CLI checks `state` (so a URL from another login attempt is rejected), exchanges the code with the PKCE verifier and prints the result JSON above. The code can be used once, and only together with the verifier held by this process. Even so, send the pasted URL only over a private channel.

Tokens land in `$EMAIL_CONFIG_DIR/accounts/work/msal.json` (mode 0600; the directory is 0700). In the Pod that is the PVC (`/opt/data/.config/email/`), so they survive restarts.

### Device code (`--flow device`)

`--flow device` prints `verification_uri` and `user_code` instead. It only works in tenants that do not block device code sign-in. tronai.com.au uses Security Defaults, which block it (`AADSTS530035`).

## Staying logged in

- Every command refreshes the access token silently. Entra refresh tokens are valid for 90 days and are renewed on each use, so an account used at least every few weeks stays logged in.
- When Entra demands a new sign-in (password reset, revoked sessions, Conditional Access, MFA re-prompt), commands exit **3** with a message like:

  ```
  token refresh failed (invalid_grant AADSTS700082); run `email auth login -a work --provider msgraph --mode draft`
  ```

  Run the login command again; the alias, tenant and client ID are reused.

## Enabling send

`email send` needs a token that carries `Mail.Send`. Turning it on is a deliberate, three-part change:

1. Entra: on the app → **API permissions** → **Add a permission** → Microsoft Graph → Delegated → `Mail.Send`, then **Grant admin consent**.
2. Log in again with `--mode send`. A draft-mode login never asks for `Mail.Send`.
3. Set `[default] mode = "send"` in the policy file, and review the `[send]` rules (`allowed_recipient_domains`, `max_recipients`, `require_reply_context`).

The effective mode is the lower of the policy mode and the login mode, so any one of the three left undone keeps sending off. After step 1, a draft-mode token also carries `Mail.Send`, because Entra includes every consented permission. Login warns about this. Do step 1 only when you mean to send.

## Troubleshooting

| Error | Cause / fix |
| --- | --- |
| `AADSTS50011` (redirect URI mismatch) after signing in | `http://localhost/callback` is not registered under **Mobile and desktop applications** (step 4). |
| `AADSTS7000218` (client_assertion or client_secret required) | **Allow public client flows** is off (step 5). |
| `AADSTS530035` (blocked by security defaults) | You used `--flow device`. Use the default auth code flow. |
| `AADSTS700016` / `AADSTS700038` | Wrong `--client-id`, or the app is registered in another tenant than `--tenant`. |
| `AADSTS65001` (consent required) | User consent is disabled in the tenant; an admin must click **Grant admin consent** (step 7). |
| `AADSTS50020` | The user is not in this tenant (single-tenant app). |
| `state mismatch` (exit 3) | The pasted URL belongs to an earlier login attempt. Start again and paste the newest URL. |
| `code redemption failed … invalid_grant` | The code expired or was already used (it is valid for a few minutes). Run login again. |
| `ErrorAccessDenied` (exit 5) on draft commands | The account was logged in with `--mode read`; log in again with `--mode draft`. |
| login warning about `Mail.Send` | Remove `Mail.Send` from the app's API permissions (and revoke consent) to keep draft mode unable to send. |
