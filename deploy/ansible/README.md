# Ansible: mavenrepo fleet deployment

Deploys `mavenrepo` (as a systemd service on loopback) plus nginx (TLS
termination + reverse proxy, from [`../nginx/`](../nginx/)) to every host in
the `mavenrepo_servers` group — currently just `mvn-dev-repo.slab.com`. This
supersedes the manual copy/edit/install steps in `deploy/nginx/README.md` for
any host managed by this playbook; that doc is still accurate for a one-off,
hand-installed box.

## Layout

| Path | Purpose |
|---|---|
| `inventory.ini` | Hosts in the `mavenrepo_servers` group |
| `playbook.yml` | The whole deploy: build, install, configure, start |
| `group_vars/mavenrepo_servers/vars.yml` | Non-secret defaults (domain, ports, paths, TLS mode) |
| `group_vars/mavenrepo_servers/vault.yml.example` | Template for the vaulted secrets file (copy → fill in → `ansible-vault encrypt`) |
| `templates/*.j2` | systemd units + nginx vhost, rendered per-host |
| `files/` | Build output lands here (`mavenrepo-linux-amd64`) — gitignored, not checked in |
| `publish-artifact.yml` | Publishes one artifact version by placing files directly + running `mavenrepo -regen` — the operator's actual "add an artifact" workflow |
| `group_vars/mavenrepo_lb/vars.yml` | Non-secret LB config (`mavenrepo_lb_port`, `mavenrepo_lb_backend_port`) — only read for hosts in the optional `mavenrepo_lb` group |
| `templates/mavenrepo-lb-stream.conf.j2` | nginx `stream{}` TCP-passthrough load balancer config — only deployed to `mavenrepo_lb` hosts |

## What changed in the repo to make this possible

1. **`-pass-file` flag added to `main.go`.** The server only took the write
   password as a bare `-pass` flag before, which would've sat in plaintext in
   the systemd unit and been visible to any local user via `ps`/`/proc`.
   `-pass-file` points at a root-owned, mode-0600 file instead; the playbook
   writes the vaulted password there rather than putting it on the command
   line. `-pass` still works for quick manual runs.
2. **`deploy/nginx/renew-cert.sh` now reads `DOMAIN`/`CA_ENDPOINT`/
   `AUTH_TOKEN_FILE` from the environment** (falling back to the same
   defaults as before) instead of having them hardcoded. That lets the exact
   same script file be copied to every host unmodified — per-host values come
   from the templated systemd unit's `Environment=` lines.
3. **A systemd unit for `mavenrepo` itself didn't exist yet** (only the nginx
   cert-renewal timer did) — added as `templates/mavenrepo.service.j2`, run
   under a dedicated non-root `mavenrepo` system user with `ProtectSystem=strict`.
4. **nginx vhost is now templated** (`templates/mavenrepo.conf.j2`) instead of
   the static `deploy/nginx/mavenrepo.conf`, which needed hand-editing the
   hostname per host — doesn't scale across a fleet.
5. **`-read-only` flag added to `main.go`, enforced at nginx too.** This repo
   holds artifacts an operator vets and publishes manually (e.g. a downloaded
   `erlang-otp` release, not something on Maven Central), and `mvn deploy`
   credential management (rotation, distributing them to every dev/CI job,
   revocation) isn't worth the overhead at this write volume. So PUT/DELETE
   are rejected outright (405) regardless of credentials, at two layers:
   `-read-only` at mavenrepo itself, and a `limit_except GET HEAD { deny
   all; }` block in `templates/mavenrepo.conf.j2` so nginx never even
   forwards a write to the backend. Set `mavenrepo_read_only: false` in
   `vars.yml` if `mvn deploy` credential management ever stops being the
   harder problem for a given host.
6. **`-regen <version-dir>` flag added to `main.go`.** The server's core
   behavior is regenerating `maven-metadata.xml` after every `PUT` — that
   never happens for files placed directly on disk, so `<latest>`/`<release>`/
   `<versions>` would silently go stale. `-regen` runs that same regeneration
   logic as a one-shot CLI command and exits; `publish-artifact.yml` calls it
   after copying files into place. Because `-read-only` hosts have no HTTP
   write path at all, `-regen` (run via this playbook) is the *only* writer —
   no race with the server's internal mutex is possible.

## Variables

Set in `group_vars/mavenrepo_servers/vars.yml` (non-secret, committed):

- `mavenrepo_domain` — `mvn-dev-repo.slab.com`
- `mavenrepo_tls_mode` — `self_signed` (default, for now) or `internal_ca`.
  Flip this once the CA's actual REST API contract is filled in in
  `../nginx/renew-cert.sh` (still has `TODO`s).
- `mavenrepo_bind_port`, `mavenrepo_install_dir`, `mavenrepo_data_dir`,
  `mavenrepo_system_user`/`_group`, `mavenrepo_read_auth`,
  `mavenrepo_self_signed_days`, `ca_endpoint`
- `mavenrepo_read_only` — `true` by default (matches this repo's actual usage:
  operator-published artifacts only, no `mvn deploy`). Set `false` for a host
  that should accept normal authenticated `mvn deploy` uploads over HTTP.

Secrets (vaulted, **not** committed):

- `mavenrepo_deploy_user` / `mavenrepo_deploy_password` — Basic auth for
  writes. **Only needed if `mavenrepo_read_only: false`** — a fully
  read-only host never provisions write credentials, since there's no HTTP
  write path for them to guard.
- `ca_api_token` — only read when `mavenrepo_tls_mode: internal_ca`

## First run

With the defaults as committed (`mavenrepo_read_only: true`,
`mavenrepo_tls_mode: self_signed`), no vault is needed at all yet:

```bash
cd deploy/ansible
ansible-playbook playbook.yml
```

Once you flip either default (accepting `mvn deploy` uploads, or switching to
the internal CA), set up the vault first:

```bash
cp group_vars/mavenrepo_servers/vault.yml.example group_vars/mavenrepo_servers/vault.yml
$EDITOR group_vars/mavenrepo_servers/vault.yml   # fill in real values
ansible-vault encrypt group_vars/mavenrepo_servers/vault.yml

ansible-playbook playbook.yml --ask-vault-pass
```

Requirements: **Go already installed on the control machine yourself** — the
playbook doesn't provision it, only checks for it and fails with a clear
message (not a bare `command not found`) if it's missing before attempting
the build. The first play cross-compiles `linux/amd64` locally, so no Go
toolchain is needed on the target hosts. Also needed: SSH + sudo access to
`mvn-dev-repo.slab.com`, and DNS for that name already pointed at the host
(outside this playbook's scope).

## Publishing an artifact

For a read-only host, this is the only way artifacts get added — there's no
`mvn deploy` path. Download the artifact yourself, then:

```bash
ansible-playbook publish-artifact.yml \
  -e mvn_group_id=org.erlang \
  -e mvn_artifact_id=otp \
  -e mvn_version=26.2.5.11 \
  -e '{"source_files": [
        {"src": "/home/operator/downloads/otp-26.2.5.11.tar.gz", "dest": "otp-26.2.5.11.tar.gz"},
        {"src": "/home/operator/downloads/otp-26.2.5.11.pom",    "dest": "otp-26.2.5.11.pom"}
      ]}'
```

This lands the files at `{{ mavenrepo_data_dir }}/org/erlang/otp/26.2.5.11/`,
writes matching `.sha1`/`.md5` sidecars (what `mvn deploy` would've produced
automatically), then runs `mavenrepo -regen org/erlang/otp/26.2.5.11` on the
host to rebuild `org/erlang/otp/maven-metadata.xml` — so `<latest>`,
`<release>`, and `<versions>` immediately reflect the new version. Maven
coordinates always split into a groupId *and* an artifactId; `org.erlang.otp`
as one string was read here as groupId `org.erlang`, artifactId `otp` — adjust
if your namespacing convention means something else.

A minimal `.pom` (with matching `groupId`/`artifactId`/`version` and
`<packaging>` set to the file's type, e.g. `tar.gz`) isn't strictly required
for the file to be servable, but most build tools expect one to resolve the
coordinate as a dependency — worth including if consumers will pull this via
`<dependency>` rather than a hardcoded URL.

## Optional: two-backend HA with a load balancer

By default this deploys one host. To run two independent backends behind a
TCP-passthrough load balancer instead:

1. **Repoint DNS.** `mavenrepo_domain` (the client-facing name, e.g.
   `mvn-dev-repo.slab.com`) needs to resolve to the *load balancer's* IP, not
   a backend's. Backends keep serving TLS for that same domain name (their
   cert's CN/SAN doesn't change) — they just aren't the thing DNS points at
   anymore.
2. **Give the two backends their own inventory host names**, distinct from
   `mavenrepo_domain` (e.g. `mvn-dev-repo-a.slab.com`,
   `mvn-dev-repo-b.slab.com` — these are only used for SSH/inventory, not
   shown to clients), and list both under `[mavenrepo_servers]` in
   `inventory.ini`. `mavenrepo_domain` in `vars.yml` stays as the single
   public name — both backends generate/request a cert for that same name.
3. **Add a third host** under `[mavenrepo_lb]` in `inventory.ini`
   (commented-out example already there).
4. `ansible-playbook playbook.yml` — the LB play matches zero hosts and does
   nothing unless `[mavenrepo_lb]` is populated, so this whole section is
   opt-in and doesn't affect the existing single-host deployment.

**How it works**: the LB's nginx uses the `stream{}` module (not `http{}`) to
proxy raw TCP — it does **not** terminate TLS or inspect the request. Each
backend's own nginx still does that independently (same
`templates/mavenrepo.conf.j2` as the single-host case: TLS termination,
`limit_except GET HEAD`). The LB just picks a backend from
`upstream mavenrepo_backends` (round-robin, passive health checks only —
open-source nginx has no active health checking) and forwards the encrypted
bytes through untouched.

**Publishing now fans out automatically.** `publish-artifact.yml` targets
`hosts: mavenrepo_servers` — with two hosts in that group, Ansible already
runs the copy/checksum/`-regen` steps against both, in parallel, per task,
and reports success/failure per host. A partial failure (one backend
succeeds, the other doesn't) is visible directly in the playbook's output;
since every step is idempotent, re-running `publish-artifact.yml` safely
reconciles a backend that failed without re-doing anything unnecessary on
the one that already succeeded. There's no two-phase-commit — this is
adequate for infrequent, human-supervised publishes, not a guarantee you'd
want for high-frequency automated writes.

**What this doesn't do**: firewall the backends' `:443` off from direct
public access (so traffic could bypass the LB entirely) — add that
separately if it matters for your network (varies by distro's
firewall manager, deliberately left out of this playbook).

## Notes

- `mavenrepo` binds to `127.0.0.1` only — nginx is the only thing that should
  ever see plaintext HTTP for this service.
- Re-running the playbook is safe: binary/unit/config changes trigger
  `restart mavenrepo` / `reload nginx` handlers; cert generation uses
  `creates:` so an existing cert is left alone (self-signed certs aren't
  re-issued on every run, and the CA-issued one is only re-fetched by the
  `cert-renew.timer`, not by re-running this playbook).
- Switching `mavenrepo_tls_mode` from `self_signed` to `internal_ca` later
  will not by itself replace an already-issued self-signed cert on disk
  (same `creates:` guard) — delete
  `/etc/nginx/certs/{{ mavenrepo_domain }}/fullchain.pem` on the host first,
  or extend the play with a `state: absent` cleanup task when doing the cutover.
