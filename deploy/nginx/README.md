# nginx + internal-CA TLS in front of mavenrepo

Terminates HTTPS at nginx and reverse-proxies to `mavenrepo` running on
loopback. Certificates come from a local, REST-API-based CA rather than
Let's Encrypt/ACME, so renewal is a small script instead of `certbot`.

For a fleet managed by Ansible (multiple hosts, per-host domain/secrets), use
[`../ansible/`](../ansible/) instead of the manual steps below — it deploys
these same files, templated per-host. This directory's manual steps are still
the right reference for a single hand-installed box, or for understanding
what the Ansible playbook is actually doing under the hood.

## Files

| File               | Purpose                                                    |
|---------------------|-------------------------------------------------------------|
| `mavenrepo.conf`    | nginx server block: HTTP→HTTPS redirect, TLS, reverse proxy |
| `renew-cert.sh`     | Generates a key+CSR, submits it to the CA API, reloads nginx |
| `cert-renew.service`/`cert-renew.timer` | systemd units that run the script daily |
| `gen-self-signed-cert.sh` | **Testing only** — generates a self-signed cert in place of the CA call |

## Defaults chosen (adjust if they don't fit)

- **TLS 1.2 + 1.3 only**, Mozilla "intermediate" cipher list, HSTS enabled
  (`max-age=15552000; includeSubDomains`).
- **`mavenrepo` bound to loopback** (`-addr 127.0.0.1:8080`, or
  `docker run -p 127.0.0.1:8080:8080 ...`) — nginx is the only thing that
  should ever see plaintext HTTP for this service.
- **Uploads**: `client_max_body_size 500m` and `proxy_request_buffering off`
  so large `mvn deploy` artifacts stream through rather than getting capped
  or double-buffered.
- **Cert renewal**: a fresh private key + CSR is generated locally on every
  run (private key never leaves the host — only the CSR is POSTed to the
  CA), key rotates every renewal, checked daily via a systemd timer.
- Basic auth for writes stays exactly as configured on `mavenrepo` itself
  (`-user`/`-pass`, or `-pass-file` to keep the password off the command line
  — recommended for anything started from a systemd unit) — nginx doesn't
  need to know about it, it's just forwarding the `Authorization` header over
  what's now an encrypted connection.
- `renew-cert.sh` reads `DOMAIN`/`CA_ENDPOINT`/`AUTH_TOKEN_FILE` from the
  environment (falling back to the placeholders below), so the same script
  file can be copied unmodified to every host — see `../ansible/` for how
  per-host values get supplied via a templated systemd unit instead of
  hand-edits.

## Testing with a self-signed cert (no CA involved)

To stand up TLS locally before the internal CA integration is wired up:

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

Swap it out for a CA-issued cert later by running `renew-cert.sh` once the
API details below are filled in — it writes to the same files.

## Setup (production, via the internal CA)

1. Edit `mavenrepo.conf` and `renew-cert.sh`: replace
   `maven.internal.example.com` with the real hostname.
2. Fill in `renew-cert.sh`'s `CA_ENDPOINT` and the request/response field
   names once the CA API's actual contract is confirmed (marked `TODO` in
   the script) — this repo's default assumes a `POST` with a `csr` field
   back a JSON body with `{"certificate": ..., "ca_chain": ...}`, which is a
   common shape but almost certainly needs adjusting.
3. `mkdir -p /etc/nginx/certs/<domain>` and drop the CA's auth token at
   `/etc/nginx/certs/ca-api-token` (`chmod 600`).
4. Run `renew-cert.sh` once manually to seed the initial cert before nginx
   starts using it.
5. `cp mavenrepo.conf /etc/nginx/sites-available/` and symlink into
   `sites-enabled/`, then `nginx -t && systemctl reload nginx`.
6. `cp renew-cert.sh /usr/local/bin/ && chmod +x /usr/local/bin/renew-cert.sh`,
   then install and enable the timer:
   ```bash
   cp cert-renew.service cert-renew.timer /etc/systemd/system/
   systemctl daemon-reload
   systemctl enable --now cert-renew.timer
   ```
