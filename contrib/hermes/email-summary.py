# Hermes webhook route script for `email watch` notifications (docs/hermes.md).
#
# Hermes runs it with the POSTed JSON on stdin, either the notification itself or
# {"<route>": <notification>}, and uses the JSON it prints as the variables for
# the route's prompt template:
#   account        alias that got the mail ("work")
#   account_label  a friendlier name, if email-summary.json defines one
#   account_flag   "-a <alias>", ready to paste into an `email` command
#   count          number of new messages (may exceed the 20 listed)
#   summary        one entry per message: id, local time, sender, subject, link
# Nothing printed means "ignore this event". Only metadata is used: watch never
# sends bodies.
#
# Optional email-summary.json next to this script (written by
# `email hermes files --timezone … --account-label …`):
#   {"timezone": "Australia/Sydney", "labels": {"work": "work mailbox (you@example.com)"}}
# Without it, times are in the system time zone and labels are the aliases.
import datetime
import json
import os
import sys

try:
    from zoneinfo import ZoneInfo
except ImportError:  # Python < 3.9
    ZoneInfo = None


def load_settings():
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "email-summary.json")
    try:
        with open(path, encoding="utf-8") as f:
            s = json.load(f)
        return s if isinstance(s, dict) else {}
    except (OSError, ValueError):
        return {}


def notification(raw):
    if isinstance(raw, dict) and raw.get("source") != "email-watch" and len(raw) == 1:
        raw = next(iter(raw.values()))
    if isinstance(raw, str):
        try:
            raw = json.loads(raw)
        except ValueError:
            return None
    return raw if isinstance(raw, dict) else None


def local_time(value, tz):
    try:
        t = datetime.datetime.fromisoformat(str(value).replace("Z", "+00:00"))
        return (t.astimezone(tz) if tz else t.astimezone()).strftime("%Y-%m-%d %H:%M")
    except (TypeError, ValueError):
        return str(value or "?")


def main():
    try:
        ev = notification(json.load(sys.stdin))
    except ValueError:
        return
    if not ev:
        return
    msgs = [m for m in ev.get("messages") or [] if isinstance(m, dict)]
    if not msgs:
        return
    settings = load_settings()
    tz = None
    if settings.get("timezone") and ZoneInfo:
        try:
            tz = ZoneInfo(str(settings["timezone"]))
        except Exception:
            tz = None
    labels = settings.get("labels") if isinstance(settings.get("labels"), dict) else {}
    account = str(ev.get("account") or "")
    lines = []
    for m in msgs[:20]:
        frm = m.get("from") or {}
        if isinstance(frm, dict):
            who = f"{frm.get('name') or ''} <{frm.get('email') or '?'}>".strip()
        else:
            who = str(frm)
        extra = ", has attachments" if m.get("has_attachments") else ""
        subject = str(m.get("subject") or "")[:120]
        lines.append(f"- id {m.get('id')}  {local_time(m.get('date'), tz)}  {who}  subject: {subject}{extra}\n"
                     f"  link: {m.get('web_link') or '-'}")
    try:
        count = int(ev.get("count") or len(msgs))
    except (TypeError, ValueError):
        count = len(msgs)
    if count > len(lines):
        lines.append(f"- {count - len(lines)} more not listed: `email -a {account} list --unread`")
    print(json.dumps({
        "account": account,
        "account_label": str(labels.get(account) or account),
        "account_flag": f"-a {account}" if account else "",
        "count": count,
        "summary": "\n".join(lines),
    }, ensure_ascii=False))


if __name__ == "__main__":
    main()
