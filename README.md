# tempmail

`tempmail` is a small, single-user web app for running temporary email addresses on your own domain.

It pairs with [Maddy](https://maddy.email/) so you can receive real internet mail, view it in a private web UI, download raw `.eml` messages, and send replies or new outbound messages from the same temporary address.

For a full VPS setup with nginx, Certbot, Maddy, DNS, and systemd, see [SETUP.md](SETUP.md).

## Features

- Receive mail for temporary addresses on your own domain.
- Send mail from opened inboxes through local Maddy submission.
- Add one or more file attachments when sending.
- Download received messages as `.eml`, including attachments.
- Open multiple domains from one binary, selected by the HTTP host.
- Temporary inbox lifecycle: close the page, close the inbox, or let it expire after the TTL.
- Temporary catch-all: open `all@example.com` to receive every address at that domain while it is active.
- Plus aliases: `test+anything@example.com` delivers to `test@example.com`.
- Dot aliases: `t.e.s.t@example.com` delivers to `test@example.com`.
- Protected inboxes: open addresses like `postmaster`, `abuse`, or `webmaster` without purging their stored mail.
- Stats page at `/mail/stats` showing active inboxes and message counts.
- Basic auth with bcrypt password hashes.
- Local-only SMTP submission and IMAP access; only public SMTP receive needs port `25`.

## What It Is For

This is intended for a private operator who wants disposable addresses on domains they control, without relying on a third-party temporary mail service.

Good uses:

- Creating short-lived inboxes for signups, testing, and verification emails.
- Temporarily catching all mail for a domain while debugging routing.
- Sending a quick reply from the same temporary address.
- Keeping permanent operational inboxes such as `postmaster` restorable and non-destructive.

This is not a multi-user hosted tempmail platform. It assumes you are the only web UI user and that the app is protected behind HTTP basic auth.

## How It Works

Maddy handles the mail-server side:

- public inbound SMTP on port `25`
- DKIM/SPF/DMARC checks
- local mailbox storage
- localhost-only submission on `127.0.0.1:587`
- localhost-only IMAP/admin access for the app

`tempmail` handles the web side:

- creates temporary Maddy credentials and mailboxes
- shows active inboxes in the browser
- reads messages from Maddy
- submits outbound messages through Maddy
- deletes non-protected temporary inboxes when they close or expire

## Address Behavior

Opening `test@example.com` creates a temporary inbox for that exact normalized address.

Aliases:

```text
test+anything@example.com -> test@example.com
t.e.s.t@example.com       -> test@example.com
```

Opening `all@example.com` creates a temporary catch-all. While it is open, it receives mail for every local address at `example.com`, including addresses that would otherwise go to a specific temporary inbox. Closing it removes the catch-all.

Protected local-parts are configurable:

```text
TEMPMAIL_PROTECTED_LOCALPARTS=postmaster,abuse,hostmaster,webmaster
```

Protected inboxes can be opened in the UI, but their mailbox contents are preserved on close/expiry. Opening a protected inbox rotates its Maddy credential for the browser session without deleting stored messages.

## Build

```sh
go build -o tempmail .
```

Generate a bcrypt admin password hash:

```sh
./tempmail -genhash
```

## Run

```sh
TEMPMAIL_DOMAINS=example.com,example.net \
TEMPMAIL_HTTP_ADDR=127.0.0.1:3005 \
TEMPMAIL_BASE_PATH=/mail \
TEMPMAIL_SUBMIT_ADDR=127.0.0.1:587 \
TEMPMAIL_MADDY_BIN=/usr/local/bin/maddy \
TEMPMAIL_ADMIN_USER=admin \
TEMPMAIL_ADMIN_PASS_HASH='bcrypt-hash-here' \
TEMPMAIL_INBOX_TTL=20m \
./tempmail
```

`TEMPMAIL_DOMAIN` is optional. If it is omitted, the first value in `TEMPMAIL_DOMAINS` is used as the fallback domain when the HTTP host does not match a configured mail domain.

## Deployment

The typical deployment is:

- nginx serves your normal website
- nginx proxies `/mail/` to `127.0.0.1:3005`
- Maddy receives public mail on `25`
- Maddy submission and IMAP stay bound to localhost
- systemd runs both `maddy` and `tempmail`

See [SETUP.md](SETUP.md) for the complete end-to-end guide.

Example config snippets live in [deploy/](deploy/).

## Outbound Delivery

Direct outbound delivery from a VPS can be rough if your IP has poor reputation or no PTR/rDNS. The setup guide includes an optional SMTP relay section for providers like SMTP2GO.

You can keep receiving mail directly on your VPS while sending outbound messages through a relay.
