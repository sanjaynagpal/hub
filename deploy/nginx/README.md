# nginx + ACME TLS in front of mavenrepo

Terminates HTTPS at nginx and reverse-proxies to `mavenrepo` running on
loopback. Certificates come from the internal CA over ACME (RFC 8555) via
`cmd/acmeclient` (in this repo's root, alongside `mavenrepo` — build with
`go build -o acmeclient ./cmd/acmeclient`), a minimal stdlib-only ACME client
using the HTTP-01 challenge. ACME is a fixed protocol, not a per-CA contract
to reverse-engineer, so this works against any conformant internal CA
(step-ca, Vault PKI's ACME support, etc.) without needing anything confirmed
about that CA's API up front — the earlier design here assumed a bespoke
REST API instead, which needed the actual request/response shape confirmed
before it could be finished; that guesswork is gone now.

For a fleet managed by Ansible (multiple hosts, per-host domain/secrets), use
[`../ansible/`](../ansible/) instead of the manual steps below — it deploys
these same files, templated per-host. This directory's manual steps are still
the right reference for a single hand-installed box, or for understanding
what the Ansible playbook is actually doing under the hood.

## Files

| File               | Purpose                                                    |
|---------------------|-------------------------------------------------------------|
| `mavenrepo.conf`    | nginx server block: HTTP→HTTPS redirect (with an ACME challenge exemption), TLS, reverse proxy |
| `../../cmd/acmeclient` | The ACME client itself — checks the installed cert's expiry, obtains/renews via HTTP-01 when needed, writes it, reloads nginx |
| `cert-renew.service`/`cert-renew.timer` | systemd units that run `acmeclient` daily |
| `notify-cert-renewal.sh.example` | Example `-notify-cmd` target — posts a Slack message for renewal_upcoming/renewal_succeeded/renewal_failed |
| `gen-self-signed-cert.sh` | **Testing only** — generates a self-signed cert in place of the ACME call |

## Defaults chosen (adjust if they don't fit)

- **TLS 1.2 + 1.3 only**, Mozilla "intermediate" cipher list, HSTS enabled
  (`max-age=15552000; includeSubDomains`).
- **`mavenrepo` bound to loopback** (`-addr 127.0.0.1:8080`, or
  `docker run -p 127.0.0.1:8080:8080 ...`) — nginx is the only thing that
  should ever see plaintext HTTP for this service.
- **Uploads**: `client_max_body_size 500m` and `proxy_request_buffering off`
  so large `mvn deploy` artifacts stream through rather than getting capped
  or double-buffered.
- **Cert renewal**: `acmeclient` checks the *installed* certificate's actual
  expiry on every run (a systemd timer triggers it daily) and only contacts
  the CA once `-renew-before-days` (default 14) are left — so it's safe to
  run daily regardless of whether the CA issues short-lived or month-scale
  certificates; most runs are a cheap local no-op. A fresh RSA-2048 private
  key + CSR is generated for each actual renewal (the key never leaves the
  host — only the CSR goes to the CA via the ACME `finalize` call). The ACME
  *account* key is separate and persists across renewals
  (`acmeclient -account-key`, default `/etc/nginx/certs/acme-account.key`)
  since it identifies the account to the CA, not the certificate. Prefer the
  old always-renew-every-run behavior instead (e.g. to lean on the CA's own
  rate limits rather than this tool's scheduling)? Set `-force`
  (`ACME_FORCE_RENEW=true`) to skip the expiry check entirely.
- **Renewal notifications**: `-notify-cmd` (empty by default) runs once as a
  heads-up `-notify-before-days` (default 21) before expiry, and again on
  each actual renewal's success or failure — see
  [`notify-cert-renewal.sh.example`](notify-cert-renewal.sh.example) and
  `cert-renew.service` for the environment variables it receives.
- Basic auth for writes stays exactly as configured on `mavenrepo` itself
  (`-user`/`-pass`, or `-pass-file` to keep the password off the command line
  — recommended for anything started from a systemd unit) — nginx doesn't
  need to know about it, it's just forwarding the `Authorization` header over
  what's now an encrypted connection.
- `acmeclient` reads `DOMAIN`/`ACME_DIRECTORY_URL`/`ACME_WEBROOT`/etc. from
  the environment (see `cmd/acmeclient/main.go`'s flag list for the full
  set, all overridable via `-flag` too), so the same binary can be copied
  unmodified to every host — see `../ansible/` for how per-host values get
  supplied via a templated systemd unit instead of hand-edits.

## Testing with a self-signed cert (no CA involved)

To stand up TLS locally before ACME issuance is wired up:

```bash
./gen-self-signed-cert.sh maven.internal.example.com   # or "localhost" for local-only testing
nginx -t && systemctl reload nginx   # or just start nginx if it isn't running yet
```

This writes `fullchain.pem`/`privkey.pem` to the exact path `mavenrepo.conf`
expects, so no other changes are needed — it's a drop-in stand-in for step 4
below. Nothing trusts a self-signed cert, so clients need an explicit
override:

- `curl -k https://maven.internal.example.com/...`
- Maven: import `fullchain.pem` into a JVM truststore and point
  `-Djavax.net.ssl.trustStore=...` at it, or set the repo's `<url>` to `http://`
  against the plaintext `mavenrepo` port while testing locally instead.
- Browser: click through the "not private" warning, or import the cert as a
  trusted root for the session.

Swap it out for a CA-issued cert later by running `acmeclient` once the
directory URL below points at the real CA — it writes to the same files.

## Setup (production, via the internal CA's ACME endpoint)

1. Edit `mavenrepo.conf`: replace `maven.internal.example.com` with the real
   hostname.
2. Build `acmeclient`: `go build -o acmeclient ./cmd/acmeclient` (from the
   repo root) and copy the resulting binary to `/usr/local/bin/acmeclient`
   on the host.
3. Find the CA's ACME directory URL (commonly `.../acme/directory` or
   similar — ask whoever runs the CA, or check its docs). If the CA requires
   external account binding (EAB — common for private CAs like step-ca),
   get a key ID + HMAC key from it too and drop the HMAC key (base64url) at
   `/etc/nginx/certs/acme-eab-key` (`chmod 600`).
4. Run once manually to seed the initial cert before nginx starts using it:
   ```bash
   DOMAIN=maven.internal.example.com \
   ACME_DIRECTORY_URL=https://ca.internal.example.com/acme/directory \
   acmeclient
   # add ACME_EAB_KID=... ACME_EAB_HMAC_KEY_FILE=/etc/nginx/certs/acme-eab-key if EAB is required
   ```
5. `cp mavenrepo.conf /etc/nginx/sites-available/` and symlink into
   `sites-enabled/`, then `nginx -t && systemctl reload nginx`.
6. Edit `cert-renew.service`'s `DOMAIN`/`ACME_DIRECTORY_URL` (and EAB lines,
   if needed) to match. Optionally set up `NOTIFY_CMD` for renewal
   notifications — copy `notify-cert-renewal.sh.example` to
   `/usr/local/bin/notify-cert-renewal.sh`, fill in a webhook URL (or point
   it at a root-owned file holding one), `chmod +x`, and uncomment the
   `Environment=NOTIFY_CMD=...` line. Then install and enable the timer:
   ```bash
   cp cert-renew.service cert-renew.timer /etc/systemd/system/
   systemctl daemon-reload
   systemctl enable --now cert-renew.timer
   ```
