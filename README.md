# tempmail

Small single-user temporary mail web app.

For a full VPS setup with nginx, Certbot, Maddy, DNS, and systemd, see [SETUP.md](SETUP.md).

It exposes:

- HTTP UI/API on `127.0.0.1:3005` by default.
- Inbound mailbox storage through local Maddy accounts.
- Outbound SMTP submission through local Maddy submission on `127.0.0.1:587` by default.

The app keeps inboxes in memory. Closing or switching the selected address deletes that inbox. A TTL reaper removes inactive inboxes if the browser disappears without a clean close.

## Build

```sh
cd /root/tempmail
go build -o tempmail .
```

## Run

```sh
TEMPMAIL_DOMAIN=example.com \
TEMPMAIL_DOMAINS=example.com,example.net \
TEMPMAIL_HTTP_ADDR=127.0.0.1:3005 \
TEMPMAIL_SUBMIT_ADDR=127.0.0.1:587 \
TEMPMAIL_MADDY_BIN=/usr/local/bin/maddy \
TEMPMAIL_ADMIN_USER=admin \
TEMPMAIL_ADMIN_PASS_HASH='bcrypt-hash-here' \
./tempmail
```

For a public domain, `TEMPMAIL_DOMAIN` should be the primary address domain users type after `@`. `TEMPMAIL_DOMAINS` can list every domain this single process should serve. If `TEMPMAIL_DOMAIN` is omitted, the first value in `TEMPMAIL_DOMAINS` is used as the primary domain.

## Maddy role

Use Maddy as the public MX, DKIM signer, SPF/DMARC checker, mailbox store, and outbound queue. The app creates temporary Maddy credentials/mailboxes and submits outbound mail through a localhost-only submission listener.
