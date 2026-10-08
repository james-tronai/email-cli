# Microsoft 365 setup

`email` uses Microsoft Graph with **delegated** permissions and the **device code** flow: it acts as one signed-in user and never holds a client secret. Setup has two parts: register an app in Entra once per tenant, then log in once per mailbox.

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
4. **Authentication** → **Add a platform** → **Mobile and desktop applications** → tick `https://login.microsoftonline.com/common/oauth2/nativeclient` → **Configure**.
5. Still on **Authentication**, under **Advanced settings**, set **Allow public client flows** to **Yes**. Save. Without this, login fails with `AADSTS7000218`.
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

## 2. Log in a mailbox

```sh
email auth login -a work --provider msgraph --mode draft \
  --tenant <directory-tenant-id> --client-id <application-client-id>
```

- `-a work` is the local alias. Pass it later with `-a`, or rely on `default_account`. The first account becomes the default.
- `--mode` decides which scopes are requested: `read` = `User.Read Mail.Read offline_access`; `draft` adds `Mail.ReadWrite`; `send` adds `Mail.Send`. Moving to a higher mode means logging in again.
- `--tenant` defaults to `organizations` (any work or school account). `common` and `consumers` are refused: personal Microsoft accounts are not supported.
- `--tenant` and `--client-id` are saved in `account.json`, so a later re-login only needs `-a work --provider msgraph --mode …`.

The command prints the device code to **stdout** at once, then waits (up to 15 minutes):

```json
{
  "verification_uri": "https://microsoft.com/devicelogin",
  "user_code": "ABCD-EFGH",
  "expires_in": 899,
  "message": "To sign in, use a web browser to open the page https://microsoft.com/devicelogin and enter the code ABCD-EFGH to authenticate."
}
```

Open the URL in any browser, on any device, enter the code and sign in as the mailbox owner. When the browser says you can close the window, the command prints the result:

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

## 3. Headless login (Kubernetes / kubectl exec)

The device code goes to stdout as JSON, so whoever runs the command (a person or an agent) can relay it:

```sh
kubectl -n hermes exec deploy/hermes-james -c hermes -- \
  email auth login -a work --provider msgraph --mode draft \
    --tenant <tenant-id> --client-id <client-id>
```

- No `-it` is needed: nothing is read from stdin.
- Send the `verification_uri` and `user_code` to the mailbox owner (for example in a Discord DM). The owner signs in from their phone or laptop. The `exec` session must stay open until the result JSON appears (at most 15 minutes, the code's lifetime).
- To print only the code line: add `--jq 'select(.user_code) | "\(.verification_uri) \(.user_code)"'`.
- Tokens land in `$EMAIL_CONFIG_DIR/accounts/work/msal.json` (mode 0600; the directory is 0700). In the Pod that is the PVC (`/opt/data/.config/email/`), so they survive restarts.

## Staying logged in

- Every command refreshes the access token silently. Entra refresh tokens are valid for 90 days and are renewed on each use, so an account used at least every few weeks stays logged in.
- When Entra demands a new sign-in (password reset, revoked sessions, Conditional Access, MFA re-prompt), commands exit **3** with a message like:

  ```
  token refresh failed (invalid_grant AADSTS700082); run `email auth login -a work --provider msgraph --mode draft`
  ```

  Run the login command again; the alias, tenant and client ID are reused.

## Troubleshooting

| Error | Cause / fix |
| --- | --- |
| `AADSTS7000218` (client_assertion or client_secret required) | **Allow public client flows** is off (step 5). |
| `AADSTS700016` / `AADSTS700038` | Wrong `--client-id`, or the app is registered in another tenant than `--tenant`. |
| `AADSTS65001` (consent required) | User consent is disabled in the tenant; an admin must click **Grant admin consent** (step 7). |
| `AADSTS50020` | The user is not in this tenant (single-tenant app). |
| `ErrorAccessDenied` (exit 5) on draft commands | The account was logged in with `--mode read`; log in again with `--mode draft`. |
| login warning about `Mail.Send` | Remove `Mail.Send` from the app's API permissions (and revoke consent) to keep draft mode unable to send. |
