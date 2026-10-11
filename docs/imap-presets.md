# IMAP presets

`--provider imap` logs in with an address and a password (usually an app password) read from stdin. `--preset` fills in the servers:

```sh
printf '%s\n' "$APP_PASSWORD" | email auth login -a mail --provider imap --mode draft \
  --username you@icloud.com --preset icloud --password-stdin
email doctor -a mail
```

Without `--preset`, addresses at the domains listed below pick their preset automatically (`gmail.com` → `gmail`). For any other server, use `--host mail.example.com` (IMAP 993 and SMTP 465 on that host), or set `--imap-host/--imap-port/--smtp-host/--smtp-port` yourself. Explicit flags always win over a preset.

**TLS** follows from the port. 993 (IMAP) and 465 (SMTP) are implicit TLS; 143, 587 and 25 use STARTTLS. Plain text is never used.

| preset | IMAP | SMTP | domains picked automatically | password |
| --- | --- | --- | --- | --- |
| `gmail` | imap.gmail.com:993 | smtp.gmail.com:465 | gmail.com, googlemail.com | app password ([details](gmail-personal-setup.md)) |
| `icloud` | imap.mail.me.com:993 | smtp.mail.me.com:587 (STARTTLS) | icloud.com, me.com, mac.com | app-specific password |
| `fastmail` | imap.fastmail.com:993 | smtp.fastmail.com:465 | fastmail.com, fastmail.fm | app password |
| `aliyun` | imap.qiye.aliyun.com:993 | smtp.qiye.aliyun.com:465 | (company domains: pass `--preset aliyun`) | login password, or the 三方客户端安全密码 |
| `qq` | imap.qq.com:993 | smtp.qq.com:465 | qq.com, foxmail.com | 授权码 |
| `163` | imap.163.com:993 | smtp.163.com:465 | 163.com | 授权码 |
| `mailu` | `--host`:993 | `--host`:465 | (self-hosted: pass `--preset mailu --host …`) | account password or a Mailu authentication token |
| `outlook` | – | – | outlook.com, hotmail.com, live.com, msn.com | **not possible**, see below |

`email doctor -a <alias>` shows what the server supports, and `--idle-test 60s` checks that it really pushes new mail. Only Gmail has `X-GM-EXT-1`. On every other server:
- ids are `<uidvalidity>.<uid>.<mailbox>`;
- threads are rebuilt from `References`/`In-Reply-To`;
- folders replace labels (`email label` is unsupported; use `move`);
- `search` takes `from: to: cc: subject: body: in:<folder> is:unread since:/before:YYYY-MM-DD` plus free words;
- there is no `web_link`.

## Per provider

### gmail

- **Password**: needs 2-Step Verification, then an app password from [myaccount.google.com/apppasswords](https://myaccount.google.com/apppasswords). The spaces Google shows are removed for you.
- **Extensions**: all of them (`X-GM-EXT-1`), so ids, threads, labels, `web_link` and search all work as they do in the Gmail web client.
- **Full walkthrough**: [gmail-personal-setup.md](gmail-personal-setup.md).

### icloud

- **Password**: [account.apple.com](https://account.apple.com) → Sign-In and Security → App-Specific Passwords. Needs two-factor authentication.
- **Username**: use the full address (`you@icloud.com`).
- **SMTP** is port 587 with STARTTLS; Apple has no implicit-TLS submission port.

### fastmail

- **Password**: Settings → Privacy & Security → Manage app passwords. The account password does not work over IMAP.
- **Plan**: IMAP and SMTP are not included in the Basic plan.

### aliyun (阿里企业邮箱)

- **Which product**: these are the Alibaba Mail for business servers. A personal `@aliyun.com` mailbox uses `imap.aliyun.com` / `smtp.aliyun.com`; pass those with `--imap-host/--smtp-host`.
- **Third-party clients may be blocked**: new organisations block them by default. An admin allows them under 邮箱管理后台 → 安全管理 → 账号安全 → 访问策略. The change takes about 5 minutes.
- **Password**: if the organisation enforces 三方客户端安全密码, generate one in web mail and use it instead of the login password.
- **IDLE**: not tested yet. Run `email doctor --idle-test 60s`; if it reports that the server does not push, run watch with `--no-idle`.

### qq (QQ邮箱)

- **Password**: web mail → 设置 → 账号 → 开启 IMAP/SMTP 服务. This shows a 授权码; use it as the password.
- **IDLE**: not tested yet; check with `email doctor --idle-test 60s` as above.

### 163 (网易163邮箱)

- **Password**: web mail → 设置 → POP3/SMTP/IMAP → 开启 IMAP/SMTP 服务, which gives a 授权码.
- **IMAP ID**: NetEase refuses to open mailboxes ("Unsafe Login") until the client identifies itself with the IMAP `ID` command. `email` sends `ID` after every login (name `email-cli`, no user or host data).

### mailu (self-hosted)

```sh
email auth login -a mail --provider imap --preset mailu --host mail.example.com \
  --username you@example.com --mode draft --password-stdin
```

- **Ports**: IMAP 993 and SMTP 465, both implicit TLS, which is what Mailu's front end exposes by default. If you only publish 587, add `--smtp-port 587`.
- **Password**: the account password works. A Mailu **authentication token** (web UI → Settings → Authentication tokens) is better: it can be revoked on its own, and you can restrict it to the IP address the agent connects from.
- **Certificate**: the server certificate must be valid for `--host` (for example from Mailu's Let's Encrypt). Self-signed certificates are refused.

### outlook: not supported

Since 16 September 2024, Outlook.com, Hotmail and Live accounts accept only OAuth for IMAP and SMTP. App passwords no longer help, so `email` refuses these addresses and `--preset outlook` with an explanation instead of failing at login.
- **Work or school Microsoft 365 accounts**: use `--provider msgraph` ([microsoft-365-setup.md](microsoft-365-setup.md)).
- **Personal Microsoft accounts**: not supported.
