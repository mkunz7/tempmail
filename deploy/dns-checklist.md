# DNS checklist

Replace `example.com`, `mail.example.com`, and IPs with your real values.

1. Hostname A/AAAA:
   - `mail.example.com. A <server IPv4>`
   - `mail.example.com. AAAA <server IPv6>` if you accept IPv6 mail.

2. Domain MX:
   - `example.com. MX 10 mail.example.com.`

3. Reverse DNS/PTR:
   - Ask your VPS/provider panel to set `<server IPv4> -> mail.example.com`.
   - If using IPv6 for SMTP, set IPv6 PTR too.

4. SPF:
   - `example.com. TXT "v=spf1 mx -all"`
   - If you send from multiple hosts, use a less strict record that includes them.

5. DKIM:
   - Generate with Maddy and publish the selector it gives you.
   - Typical shape: `default._domainkey.example.com. TXT "v=DKIM1; k=ed25519; p=..."`

6. DMARC:
   - Start gentle: `_dmarc.example.com. TXT "v=DMARC1; p=none; rua=mailto:postmaster@example.com"`
   - After testing: change `p=quarantine` or `p=reject`.

7. TLS policy, optional:
   - MTA-STS/TLS-RPT can be added after basic mail flow works.

8. Firewall:
   - Public inbound: TCP 25 for SMTP and 80/443 for nginx/certbot.
   - Local-only: keep Maddy submission `587`, Maddy IMAP `143`, and tempmail `3005` bound to `127.0.0.1`.
