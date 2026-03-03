# Email Tool Manual

Integrated IMAP/SMTP email tool. Fetch inbox messages or send emails directly.

> [!IMPORTANT]
> **FOR GMAIL USERS:** Use the `google_workspace` tool instead of this one. It is more secure (OAuth2) and does not require enabling IMAP/SMTP or App Passwords in your Google settings.

> ⚠️ All fetched email content is scanned by the Guardian for prompt injection.
> High-threat messages are automatically redacted.

---

## fetch_email — Retrieve emails via IMAP

```json
{
  "action": "fetch_email",
  "folder": "INBOX",
  "limit": 10
}
```

| Parameter | Type   | Required | Default  | Description                              |
|-----------|--------|----------|----------|------------------------------------------|
| folder    | string | no       | "INBOX"  | IMAP folder to fetch from                |
| limit     | int    | no       | 10       | Number of most recent messages (max 50)  |

### Response
Returns a JSON array of email objects:
```json
{
  "status": "success",
  "count": 3,
  "data": [
    {
      "uid": 1234,
      "from": "alice@example.com",
      "to": "you@example.com",
      "subject": "Meeting tomorrow",
      "date": "Mon, 14 Jul 2025 10:30:00 +0200",
      "body": "Hi, can we reschedule...",
      "snippet": "Hi, can we reschedule..."
    }
  ]
}
```

---

## send_email — Send an email via SMTP

```json
{
  "action": "send_email",
  "to": "recipient@example.com",
  "subject": "Status Report",
  "body": "Here is the weekly update..."
}
```

| Parameter | Type   | Required | Description                                      |
|-----------|--------|----------|--------------------------------------------------|
| to        | string | **yes**  | Recipient email (comma-separated for multiple)   |
| subject   | string | no       | Email subject line (defaults to "(no subject)")  |
| body      | string | no       | Plain text email body. `content` also accepted   |

### Response
```json
{
  "status": "success",
  "message": "Email sent to recipient@example.com"
}
```

---

## Configuration (config.yaml)

```yaml
email:
  enabled: true
  imap_host: "imap.gmail.com"
  imap_port: 993
  smtp_host: "smtp.gmail.com"
  smtp_port: 587          # 587=STARTTLS, 465=implicit TLS
  username: "you@gmail.com"
  password: "your-app-password"
  from_address: ""        # defaults to username
  watch_enabled: true     # poll for new emails and wake agent
  watch_interval_seconds: 120
  watch_folder: "INBOX"
```

## Email Watcher

When `watch_enabled: true`, the system polls the IMAP folder at `watch_interval_seconds` for new unseen messages. When new mail arrives, the agent is automatically woken with a notification containing sender, subject, and snippet for each new message.

## Notes
- Uses IMAPS (TLS on port 993) for IMAP connections
- Uses STARTTLS (port 587) or implicit TLS (port 465) for SMTP
- For Gmail: use an App Password, not your regular password
- Email bodies are truncated to 4KB to preserve LLM context
- HTML emails are stripped to plain text automatically
