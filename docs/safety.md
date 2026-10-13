# Safety model

What the agent can do is decided by you, the deployer, in a policy file the agent cannot change. Every write and every denial is logged. Mail content is treated as data, never as instructions.

## Policy and content

- **Modes** `read` < `draft` < `send`. The effective mode is the lower of the policy mode and the mode the account logged in with. Login only requests the scopes that mode needs, so a draft-mode M365 token has no `Mail.Send`. Gmail is different: no Gmail scope can create drafts without also being able to send (`gmail.modify` can send), so on Gmail only the CLI's policy gate blocks sending. An `imap` app password can do everything; there the client enforces the mode itself (no changes below `draft`, and no SMTP connection below `send`).
- **Policy file**: `$EMAIL_POLICY_FILE`, else `~/.config/email/policy.toml` (annotated example: [policy.example.toml](policy.example.toml)). A missing file means `read`. An unparsable file, or one with unknown keys, stops every command with exit 4. The policy binds the agent only where the agent cannot edit it. The Helm chart mounts it read-only into Hermes. On one machine the agent's terminal runs as you and could rewrite it, so there the provider-side limit matters: a Microsoft 365 login with `--mode draft` has no `Mail.Send`, whereas a Gmail token or an app password can always send.
- **Audit**: every write and every denial is appended to `~/.local/state/email/audit.jsonl`.
- **Untrusted content**:
  - Message bodies always go in `body_text`.
  - Text output wraps bodies in `BEGIN/END UNTRUSTED MESSAGE BODY` fences; a fence marker inside a body is defused.
  - Hidden HTML text (white-on-white, `display:none`, zero font size, …) is stripped and flagged with `body_hidden_text_removed`. When a message has HTML, the text is derived from that HTML rather than from the sender's `text/plain` part, so the agent reads what a human would see.
  - Bodies are truncated at 20 000 characters.
  - Attachments are only saved by an explicit `download`, with sanitized names and mode 0600.
- **Trash and delete** are disabled unless `[delete] enabled = true`. Draft deletion is always allowed.
- **Policy templates**: [policy/](policy/) has `read`, `draft` (recommended to start), `send-reply-only` and `send-open`. `email policy show` prints the active policy; `email capabilities` lists what the current account may do under it.

## Sending

`email send <draft-id>` sends a draft that already exists. There is no command that composes and sends in one step. The body goes out exactly as it is on the server: the signoff was added when the draft was written.

- **When it is available**: only when the policy says `mode = "send"` **and** the account logged in with `--mode send`. Otherwise it exits 4 before any network call.
- **Recipient rules**: checked against the draft as it is on the server at send time (to + cc + bcc), so edits made after the draft was created count. The rules come from `[send]` in the policy file:
  - `allowed_recipient_domains`: every recipient must be in one of these domains. An empty list means no restriction.
  - `max_recipients`: at most this many recipients. 0 means unlimited.
  - `require_reply_context`: the draft must be a reply or forward, meaning its thread holds a message that is not a draft. A brand-new message is refused.
- **Idempotency**: `--idempotency-key` defaults to the draft id. A key that was sent within the last 24 hours returns the first result with `"replayed": true` and sends nothing. While a send with the same key is running, another one exits 5 with code `send_in_progress`. Records live in `<state dir>/send/`.
- **Audit**: every send is written to `audit.jsonl` with its recipients and the sent message id. So is every denial and every replay.
- **Enabling it**:
  - Microsoft 365: add `Mail.Send` to the Entra app (with admin consent), log in again with `--mode send`, and set the policy mode to `send`.
  - Gmail: the draft-mode token can already send, so the policy mode is the only switch.
  - imap: log in again with `--mode send` (same app password) and set the policy mode to `send`. SMTP (`smtp.gmail.com:465` for Gmail) is used only then.

