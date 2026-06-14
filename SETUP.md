# Tempmail VPS Setup Guide

This guide sets up a single-user temporary email web app on a VPS using:

- Nginx for HTTPS web hosting.
- Certbot for TLS certificates.
- Maddy for receiving mail, DKIM signing, local mailbox storage, and outbound SMTP submission.
- The `tempmail` Go binary for the private web UI.

The examples use:

- Web domain: `example.com`
- Mail hostname: `mail.example.com`
- Tempmail URL: `https://example.com/mail/`
- App listener: `127.0.0.1:3005`

Replace these with your real domain names.

## 1. VPS prerequisites

Start with a VPS where you can SSH as root or a sudo user.

Open only the ports you need:

```sh
ufw allow OpenSSH
ufw allow 80/tcp
ufw allow 443/tcp
ufw allow 25/tcp
ufw enable
```

Port use:

- `80/tcp`: Certbot HTTP challenge and HTTP redirect.
- `443/tcp`: HTTPS website.
- `25/tcp`: public SMTP receive from other mail servers.
- `587/tcp`: not public in this setup; Maddy binds it to localhost only.
- `143/tcp`: not public in this setup; Maddy binds it to localhost only.

Install packages:

```sh
apt update
apt install -y nginx certbot python3-certbot-nginx golang git sqlite3 acl zstd
```

## 2. DNS records

Create these DNS records at your DNS provider.

For one mail domain:

```text
example.com.       A      YOUR_VPS_IPV4
mail.example.com.  A      YOUR_VPS_IPV4
example.com.       MX 10  mail.example.com.
example.com.       TXT    "v=spf1 mx -all"
_dmarc.example.com TXT    "v=DMARC1; p=none; rua=mailto:postmaster@example.com"
```

After Maddy creates your DKIM key, add the DKIM TXT record it prints or stores. A common selector in this guide is `default`, so the DNS name will usually be:

```text
default._domainkey.example.com TXT "v=DKIM1; k=ed25519; p=..."
```

Reverse DNS/PTR matters for sending mail. If your VPS provider lets you set rDNS, set the PTR for `YOUR_VPS_IPV4` to:

```text
mail.example.com
```

If you cannot set rDNS, receiving mail will still work, but outbound mail to Gmail/Outlook/Yahoo will often land in spam or be rejected. For reliable outbound delivery, use a transactional SMTP provider for sending.

## 3. Create a basic nginx site

Create the web root:

```sh
mkdir -p /var/www/example.com/html
printf '<h1>example.com</h1>\n' > /var/www/example.com/html/index.html
```

Create `/etc/nginx/sites-available/example.com`:

```nginx
server {
    listen 80;
    listen [::]:80;

    server_name example.com www.example.com;

    root /var/www/example.com/html;
    index index.html index.htm;

    location / {
        try_files $uri $uri/ =404;
    }
}
```

Enable it:

```sh
ln -s /etc/nginx/sites-available/example.com /etc/nginx/sites-enabled/example.com
nginx -t
systemctl reload nginx
```

Get HTTPS certificates for the website and mail hostname:

```sh
certbot --nginx -d example.com -d www.example.com
certbot --nginx -d mail.example.com
```

## 4. Install Maddy

Download a Maddy release from <https://github.com/foxcpp/maddy/releases>. Pick the current Linux amd64 build for your VPS.

Example:

```sh
mkdir -p /root/maddy-install
cd /root/maddy-install
wget https://github.com/foxcpp/maddy/releases/download/v0.9.5/maddy-0.9.5-x86_64-linux-musl.tar.zst
tar --zstd -xf maddy-0.9.5-x86_64-linux-musl.tar.zst
cd maddy-0.9.5-x86_64-linux-musl
install -o root -g root -m 0755 maddy /usr/local/bin/maddy
cp systemd/* /etc/systemd/system/
useradd -mrU -s /usr/sbin/nologin -d /var/lib/maddy -c "maddy mail server" maddy
mkdir -p /etc/maddy /var/lib/maddy
chown maddy:maddy /var/lib/maddy
systemctl daemon-reload
```

If your distro uses `/sbin/nologin` instead of `/usr/sbin/nologin`, either path is fine.

Allow Maddy to read Certbot certificates:

```sh
setfacl -R -m u:maddy:rX /etc/letsencrypt/live/mail.example.com /etc/letsencrypt/archive/mail.example.com
setfacl -m u:maddy:x /etc/letsencrypt /etc/letsencrypt/live /etc/letsencrypt/archive
```

Create `/etc/maddy/aliases`:

```sh
cat > /etc/maddy/aliases <<'EOF'
postmaster@example.com: postmaster@example.com
abuse@example.com: postmaster@example.com
hostmaster@example.com: postmaster@example.com
webmaster@example.com: postmaster@example.com
EOF
```

Set restricted permissions:

```sh
chown root:maddy /etc/maddy/aliases
chmod 0640 /etc/maddy/aliases
```

## 5. Configure Maddy

Create `/etc/maddy/maddy.conf`:

```maddy
$(hostname) = mail.example.com
$(primary_domain) = example.com
$(local_domains) = $(primary_domain)

tls file /etc/letsencrypt/live/mail.example.com/fullchain.pem /etc/letsencrypt/live/mail.example.com/privkey.pem

state_dir /var/lib/maddy
runtime_dir /run/maddy

auth.pass_table local_authdb {
    table sql_table {
        driver sqlite3
        dsn credentials.db
        table_name passwords
    }
}

storage.imapsql local_mailboxes {
    driver sqlite3
    dsn imapsql.db
}

hostname $(hostname)

table.chain local_rewrites {
    optional_step regexp "(.+)\+(.+)@(.+)" "$1@$3"
    optional_step sql_query {
        driver sqlite3
        dsn credentials.db
        lookup "SELECT replace(substr(:key, 1, instr(:key, '@') - 1), '.', '') || substr(:key, instr(:key, '@')) WHERE instr(:key, '@') > 1 AND instr(substr(:key, 1, instr(:key, '@') - 1), '.') > 0"
    }
    optional_step sql_query {
        driver sqlite3
        dsn credentials.db
        lookup "SELECT 'all@' || substr(:key, instr(:key, '@') + 1) WHERE instr(:key, '@') > 1 AND EXISTS (SELECT 1 FROM passwords WHERE key = 'all@' || substr(:key, instr(:key, '@') + 1))"
    }
    optional_step file /etc/maddy/aliases
}

table.chain sender_rewrites {
    optional_step regexp "(.+)\+(.+)@(.+)" "$1@$3"
    optional_step sql_query {
        driver sqlite3
        dsn credentials.db
        lookup "SELECT replace(substr(:key, 1, instr(:key, '@') - 1), '.', '') || substr(:key, instr(:key, '@')) WHERE instr(:key, '@') > 1 AND instr(substr(:key, 1, instr(:key, '@') - 1), '.') > 0"
    }
    optional_step sql_query {
        driver sqlite3
        dsn credentials.db
        lookup "SELECT 'all@' || substr(:key, instr(:key, '@') + 1) WHERE instr(:key, '@') > 1 AND EXISTS (SELECT 1 FROM passwords WHERE key = 'all@' || substr(:key, instr(:key, '@') + 1))"
    }
    optional_step file /etc/maddy/aliases
}

msgpipeline local_routing {
    destination postmaster $(local_domains) {
        modify {
            replace_rcpt &local_rewrites
        }
        deliver_to &local_mailboxes
    }

    default_destination {
        reject 550 5.1.1 "User doesn't exist"
    }
}

smtp tcp://0.0.0.0:25 {
    limits {
        all rate 20 1s
        all concurrency 10
    }

    dmarc yes

    check {
        require_mx_record
        dkim
        spf
    }

    source $(local_domains) {
        reject 501 5.1.8 "Use Submission for outgoing SMTP"
    }

    default_source {
        destination postmaster $(local_domains) {
            deliver_to &local_routing
        }
        default_destination {
            reject 550 5.1.1 "User doesn't exist"
        }
    }
}

submission tcp://127.0.0.1:587 {
    limits {
        all rate 50 1s
    }

    auth &local_authdb

    source $(local_domains) {
        check {
            authorize_sender {
                prepare_email &sender_rewrites
                user_to_email identity
            }
        }

        destination postmaster $(local_domains) {
            deliver_to &local_routing
        }

        default_destination {
            modify {
                dkim $(primary_domain) $(local_domains) default
            }
            deliver_to &remote_queue
        }
    }

    default_source {
        reject 501 5.1.8 "Non-local sender domain"
    }
}

target.remote outbound_delivery {
    limits {
        destination rate 20 1s
        destination concurrency 10
    }

    mx_auth {
        dane
        mtasts {
            cache fs
            fs_dir mtasts_cache/
        }
        local_policy {
            min_tls_level encrypted
            min_mx_level none
        }
    }
}

target.queue remote_queue {
    target &outbound_delivery
    autogenerated_msg_domain $(primary_domain)

    bounce {
        destination postmaster $(local_domains) {
            deliver_to &local_routing
        }
        default_destination {
            reject 550 5.0.0 "Refusing to send DSNs to non-local addresses"
        }
    }
}

imap tcp://127.0.0.1:143 {
    auth &local_authdb
    storage &local_mailboxes
}
```

For multiple domains, expand the variables and TLS line:

```maddy
$(hostname) = mail.example.com
$(primary_domain) = example.com
$(local_domains) = $(primary_domain) example.net

tls file /etc/letsencrypt/live/mail.example.com/fullchain.pem /etc/letsencrypt/live/mail.example.com/privkey.pem /etc/letsencrypt/live/mail.example.net/fullchain.pem /etc/letsencrypt/live/mail.example.net/privkey.pem
```

Validate and start Maddy:

```sh
maddy -config /etc/maddy/maddy.conf verify-config
systemctl enable --now maddy
systemctl status maddy
```

If the service times out on startup, check whether your packaged systemd unit uses `Type=notify`. If Maddy was installed manually and notify support is not wired up, change the unit to:

```ini
Type=simple
```

Then reload and restart:

```sh
systemctl daemon-reload
systemctl restart maddy
```

## 6. Create a permanent postmaster account

Every domain should have a working postmaster address:

```sh
maddy creds create postmaster@example.com
maddy imap-acct create postmaster@example.com
```

For multiple domains, repeat this for each domain.

## 7. Find and publish DKIM

Maddy stores generated keys under `/var/lib/maddy`. Look for the public DKIM value:

```sh
find /var/lib/maddy -type f -iname '*dkim*' -o -iname '*default*'
```

If needed, inspect Maddy logs:

```sh
journalctl -u maddy -n 100 --no-pager
```

Publish the DKIM TXT record in DNS. The exact value depends on the generated key.

## 8. Build and install tempmail

Get the source onto the VPS, then build:

```sh
cd /root/tempmail
go build -o tempmail .
mkdir -p /var/www/tempmail /etc/tempmail
install -o root -g root -m 0755 /root/tempmail/tempmail /var/www/tempmail/tempmail
```

Generate an admin password hash:

```sh
/var/www/tempmail/tempmail -genhash
```

Create `/etc/tempmail/tempmail.env`:

```text
TEMPMAIL_DOMAINS=example.com
TEMPMAIL_HTTP_ADDR=127.0.0.1:3005
TEMPMAIL_BASE_PATH=/mail
TEMPMAIL_SUBMIT_ADDR=127.0.0.1:587
TEMPMAIL_MADDY_BIN=/usr/local/bin/maddy
TEMPMAIL_ADMIN_USER=admin
TEMPMAIL_ADMIN_PASS_HASH=PASTE_HASH_HERE
TEMPMAIL_PROTECTED_LOCALPARTS=postmaster,abuse,hostmaster,webmaster
TEMPMAIL_INBOX_TTL=20m
```

For multiple domains:

```text
TEMPMAIL_DOMAINS=example.com,example.net
```

`TEMPMAIL_DOMAIN` is optional. If it is omitted, the first domain in `TEMPMAIL_DOMAINS` is used as the fallback domain when the HTTP host does not match a configured mail domain.

Create `/etc/systemd/system/tempmail.service`:

```ini
[Unit]
Description=Temporary Mail Web UI
After=network-online.target maddy.service
Wants=network-online.target
Requires=maddy.service

[Service]
Type=simple
User=maddy
Group=maddy
WorkingDirectory=/var/lib/maddy
EnvironmentFile=/etc/tempmail/tempmail.env
ExecStart=/var/www/tempmail/tempmail
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

Start it:

```sh
systemctl daemon-reload
systemctl enable --now tempmail
systemctl status tempmail
```

## 9. Add nginx proxy for `/mail/`

Edit your HTTPS nginx server block for `example.com` and add this before the `location /` block:

```nginx
location = /mail {
    return 301 /mail/;
}

location /mail/ {
    proxy_pass http://127.0.0.1:3005;
    proxy_http_version 1.1;

    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-Host $host;

    client_max_body_size 35m;
}
```

Keep your normal website root for `/`:

```nginx
location / {
    try_files $uri $uri/ =404;
}
```

Validate and reload:

```sh
nginx -t
systemctl reload nginx
```

Now open:

```text
https://example.com/mail/
```

## 10. Test receiving mail

Log in to the web UI and open an inbox such as:

```text
test@example.com
```

Send mail to it from Gmail or another external mailbox.

Watch Maddy logs:

```sh
journalctl -u maddy -f
```

The app polls every few seconds and should show the message in the inbox.

## 11. Test sending mail

From the web UI, send a message to an external address.

Watch Maddy:

```sh
journalctl -u maddy -f
```

If delivery to Gmail/Outlook/Yahoo fails or lands in spam, check:

- SPF exists and includes your MX.
- DKIM TXT record is published correctly.
- DMARC exists.
- PTR/rDNS matches your mail hostname.
- Your VPS IP is not on major blocklists.

For better outbound delivery, keep Maddy for receiving and use a low-volume transactional SMTP provider for sending.

## 12. Optional: send outbound mail through SMTP2GO

You can keep this server as your inbound MX while sending outbound mail through a relay such as SMTP2GO. This helps when your VPS has poor IP reputation, blocked outbound port 25, or no configurable PTR/rDNS.

In this model:

- Other mail servers still deliver inbound mail to your VPS on port `25`.
- The tempmail app still submits outbound messages to local Maddy on `127.0.0.1:587`.
- Maddy signs/routes the message, then forwards outbound mail to SMTP2GO instead of delivering directly to remote MX servers.

### Create SMTP2GO credentials

In SMTP2GO:

1. Add and verify your sender domain under verified senders/sender domains.
2. Add the DNS records SMTP2GO gives you, usually CNAME records for SPF/return-path and DKIM.
3. Create or copy an SMTP user under `Sending > SMTP Users`.

SMTP2GO's generic SMTP settings are:

```text
Server: mail.smtp2go.com
Port: 2525
Username: your SMTP2GO SMTP username
Password: your SMTP2GO SMTP password
```

SMTP2GO also supports ports `25`, `80`, `8025`, and `587` with STARTTLS, plus implicit TLS ports `465`, `8465`, and `443`. Port `2525` is a good first choice because many VPS providers block outbound `25`.

If you use one of SMTP2GO's implicit TLS ports, use a `tls://` target in Maddy, for example `targets tls://mail.smtp2go.com:465`.

### Change Maddy to use SMTP2GO

Edit `/etc/maddy/maddy.conf`.

Keep this part in the `submission` block:

```maddy
default_destination {
    modify {
        dkim $(primary_domain) $(local_domains) default
    }
    deliver_to &remote_queue
}
```

This keeps local DKIM signing enabled before the message is handed to SMTP2GO. You can also rely on SMTP2GO's DKIM after verifying the sender domain there, but keeping local DKIM is fine as long as your DKIM DNS record is valid.

Then replace the direct-delivery `target.remote outbound_delivery { ... }` block with an SMTP relay target:

```maddy
target.smtp smtp2go_relay {
    targets tcp://mail.smtp2go.com:2525
    starttls yes
    auth plain SMTP2GO_USERNAME SMTP2GO_PASSWORD
}
```

Then change the queue target from:

```maddy
target.queue remote_queue {
    target &outbound_delivery
    autogenerated_msg_domain $(primary_domain)

    bounce {
        destination postmaster $(local_domains) {
            deliver_to &local_routing
        }
        default_destination {
            reject 550 5.0.0 "Refusing to send DSNs to non-local addresses"
        }
    }
}
```

To:

```maddy
target.queue remote_queue {
    target &smtp2go_relay
    autogenerated_msg_domain $(primary_domain)

    bounce {
        destination postmaster $(local_domains) {
            deliver_to &local_routing
        }
        default_destination {
            reject 550 5.0.0 "Refusing to send DSNs to non-local addresses"
        }
    }
}
```

Set restrictive permissions because the Maddy config now contains relay credentials:

```sh
chown root:maddy /etc/maddy/maddy.conf
chmod 0640 /etc/maddy/maddy.conf
```

Validate and restart:

```sh
maddy -config /etc/maddy/maddy.conf verify-config
systemctl restart maddy
journalctl -u maddy -f
```

Send a message from the tempmail UI to Gmail and watch the logs. If SMTP2GO rejects the message, check that:

- The sender domain is verified in SMTP2GO.
- The SMTP username/password are from `Sending > SMTP Users`.
- Your `From` address uses a verified sender domain.
- DNS records from SMTP2GO have propagated.

### DNS when using SMTP2GO

Keep your inbound records pointing at your VPS:

```text
example.com.       MX 10  mail.example.com.
mail.example.com.  A      YOUR_VPS_IPV4
```

For outbound authentication, follow the DNS records shown in SMTP2GO for your verified sender domain. Do not guess these values; SMTP2GO generates account/domain-specific CNAMEs.

You can keep your existing DMARC record. If you already have one, do not create a second DMARC record.

Your SPF record for inbound/direct server identity can stay:

```text
example.com. TXT "v=spf1 mx -all"
```

SMTP2GO's normal sender-domain verification handles its own SPF/return-path alignment through the CNAME records it gives you. Only add an `include:` to your root SPF record if SMTP2GO explicitly tells you to for your chosen setup.

## 13. Address behavior

The setup supports temporary aliases:

```text
test+anything@example.com -> test@example.com
t.e.s.t@example.com       -> test@example.com
```

Opening `all@example.com` creates a catch-all inbox. While it exists, it steals mail for that domain, including mail that would otherwise go to a specific temporary inbox. Closing `all@example.com` removes the catch-all.

Protected local parts can be opened in the UI, but their mailbox contents are preserved when the inbox is closed or expires:

```text
postmaster,abuse,hostmaster,webmaster
```

When a protected inbox is opened, the app rotates its Maddy credential to a temporary password for that browser session. The mailbox and messages are not deleted.

Customize this with:

```text
TEMPMAIL_PROTECTED_LOCALPARTS=postmaster,abuse,security
```

## 14. Useful commands

Check listeners:

```sh
ss -ltnp | grep -E ':25|:80|:443|:587|:143|:3005'
```

Expected:

- `0.0.0.0:25` public SMTP receive.
- `127.0.0.1:587` local submission only.
- `127.0.0.1:143` local IMAP only.
- `127.0.0.1:3005` local tempmail web app only.
- `0.0.0.0:80` and `0.0.0.0:443` nginx.

Reload after config changes:

```sh
maddy -config /etc/maddy/maddy.conf verify-config
systemctl restart maddy
systemctl restart tempmail
nginx -t && systemctl reload nginx
```

List Maddy accounts:

```sh
maddy creds list
```

Remove a temporary account manually:

```sh
maddy imap-acct remove -y test@example.com
maddy creds remove -y test@example.com
```

View logs:

```sh
journalctl -u maddy -f
journalctl -u tempmail -f
journalctl -u nginx -f
```

## 15. Backups

Back up:

```text
/etc/maddy/maddy.conf
/etc/maddy/aliases
/etc/tempmail/tempmail.env
/etc/systemd/system/tempmail.service
/var/lib/maddy
```

The tempmail app keeps active browser sessions in memory, so active temporary inbox state is lost on app restart. Mailbox/account data lives in Maddy's SQLite files under `/var/lib/maddy`.
