# mavenrepo — Operator Runbook

Practical, task-oriented guide for setting up and running `mvn-dev-repo.slab.com`
day to day. For *why* it's built this way, see [`architecture.md`](architecture.md).
All commands below assume you're in `deploy/ansible/` on the Ansible control
node unless stated otherwise.

## 1. Prerequisites

On the Ansible control node:
- `ansible` and `ansible-playbook` installed.
- **Go installed yourself** (the playbook doesn't install it — it only checks
  and fails with a clear message if it's missing). It cross-compiles the
  `linux/amd64` binary locally before copying it to the target — no Go
  needed on the server itself, just here.
- SSH access + sudo (`become`) on `mvn-dev-repo.slab.com`.
- DNS for `mvn-dev-repo.slab.com` already pointed at the host — not managed
  by this playbook.

## 2. First-time setup

```bash
cd deploy/ansible
ansible-playbook playbook.yml
```

With the committed defaults (`mavenrepo_read_only: true`,
`mavenrepo_tls_mode: self_signed`) this needs no vault/secrets at all. It
will:
1. Cross-compile `mavenrepo` and copy it to `/opt/mavenrepo/bin/mavenrepo`.
2. Create the `mavenrepo` system user/group and `/var/lib/mavenrepo/repo`.
3. Install and start the `mavenrepo` systemd service (loopback-bound, `-read-only`).
4. Install nginx, generate a self-signed cert, deploy the TLS vhost, and
   start nginx.

### Verify it worked

```bash
ssh mvn-dev-repo.slab.com systemctl status mavenrepo nginx
curl -k https://mvn-dev-repo.slab.com/          # -k: self-signed cert, expected for now
curl -k -X PUT https://mvn-dev-repo.slab.com/foo/bar/1.0/bar-1.0.jar -d test
# ^ expect: 405 (read-only is working)
```

## 3. Publishing an artifact

This is the *only* way artifacts get added — there is no `mvn deploy` path
on this host (see architecture.md §6 for why).

1. Download and vet the artifact yourself, locally on the control node.
2. Run the publish playbook with the coordinate and local file paths:

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

This copies the files into
`/var/lib/mavenrepo/repo/org/erlang/otp/26.2.5.11/`, writes `.sha1`/`.md5`
sidecars, and runs `mavenrepo -regen` on the host to rebuild
`org/erlang/otp/maven-metadata.xml`.

3. Verify:

```bash
curl -k https://mvn-dev-repo.slab.com/org/erlang/otp/maven-metadata.xml
# <latest> and <release> should show the new version
curl -k -I https://mvn-dev-repo.slab.com/org/erlang/otp/26.2.5.11/otp-26.2.5.11.tar.gz
# expect 200
```

**Coordinate note**: Maven always needs a groupId *and* an artifactId. A
single dotted string like `org.erlang.otp` needs to be split — the example
above uses groupId `org.erlang`, artifactId `otp`. Confirm this matches your
namespacing convention before publishing.

**Including a POM**: not strictly required for the file to be servable, but
most build tools expect one to resolve the coordinate as a `<dependency>`
rather than a hardcoded URL. Include a minimal one with matching
`groupId`/`artifactId`/`version` and `<packaging>` set to the artifact's file
type (e.g. `tar.gz`) if consumers will depend on it that way.

## 4. Removing or replacing an artifact version

There's no `DELETE` endpoint available either (also blocked by `-read-only`).
Do it directly on the host:

```bash
ssh mvn-dev-repo.slab.com
sudo su - mavenrepo -s /bin/bash    # or: sudo -u mavenrepo -i

rm -rf /var/lib/mavenrepo/repo/org/erlang/otp/26.2.5.11

# Rebuild the artifact-level metadata so <versions>/<latest>/<release> no
# longer reference the removed version:
/opt/mavenrepo/bin/mavenrepo -root /var/lib/mavenrepo/repo \
  -regen org/erlang/otp/<some-other-remaining-version>
```

`-regen` takes a *version directory* and rebuilds the metadata for its
parent artifact directory — point it at any version that still exists under
that artifact so the artifact-level file gets rewritten. If you just removed
the *only* version of an artifact, there's nothing left to regenerate from;
also remove the now-empty `org/erlang/otp/maven-metadata.xml*` files by hand.

To replace a version's contents in place, just re-run `publish-artifact.yml`
with the same coordinate — the copy overwrites existing files, and `-regen`
picks up whatever's on disk afterward either way.

## 5. Redeploying config/binary changes

Re-running the main playbook is always safe:

```bash
ansible-playbook playbook.yml
```

- Binary or systemd unit changes → `mavenrepo` restarts automatically
  (Ansible handler).
- nginx vhost changes → nginx reloads automatically.
- An already-issued cert is left alone (`creates:` guard) — see §6 to force
  cert renewal.
- Artifacts already on disk are untouched; this playbook never touches
  `mavenrepo_data_dir` contents.

## 6. Certificate management

### Self-signed (current default)

Valid for `mavenrepo_self_signed_days` (825 by default) from issuance.
Nothing trusts it — clients need `curl -k`, a Maven truststore import, or a
browser click-through. To force reissue (e.g. it's about to expire, or the
domain changed):

```bash
ssh mvn-dev-repo.slab.com sudo rm -rf /etc/nginx/certs/mvn-dev-repo.slab.com
ansible-playbook playbook.yml
```

(The `creates:` idempotency guard only skips generation if the file's
already there — deleting it first makes the playbook regenerate it.)

### Switching to the internal CA

Not yet available — `deploy/nginx/renew-cert.sh` has unresolved `TODO`s for
the CA's actual API contract (endpoint, auth, request/response shape). Once
those are filled in:

1. Set up the vault (see §7).
2. Set `mavenrepo_tls_mode: internal_ca` in
   `group_vars/mavenrepo_servers/vars.yml`.
3. Remove the existing self-signed cert directory (as above) so the new mode
   isn't blocked by the old cert's `creates:` guard.
4. Re-run `ansible-playbook playbook.yml --ask-vault-pass`.

Renewal after that point is automatic — `cert-renew.timer` runs
`renew-cert.sh` daily on the host.

## 7. Setting up the vault (only needed if you change a default)

Not required for the committed defaults. Needed if you set
`mavenrepo_read_only: false` (write credentials) or
`mavenrepo_tls_mode: internal_ca` (CA API token):

```bash
cp group_vars/mavenrepo_servers/vault.yml.example group_vars/mavenrepo_servers/vault.yml
$EDITOR group_vars/mavenrepo_servers/vault.yml
ansible-vault encrypt group_vars/mavenrepo_servers/vault.yml
ansible-playbook playbook.yml --ask-vault-pass
```

## 8. Troubleshooting

| Symptom | Likely cause / check |
|---|---|
| `curl` fails TLS verification | Expected with the self-signed default — use `-k`, or see §6 |
| `PUT`/`DELETE` returns 405 | Expected — this host is `-read-only` by design, not a bug. Use `publish-artifact.yml` (§3) instead |
| `mavenrepo` service won't start | `journalctl -u mavenrepo -e`; check `/var/lib/mavenrepo/repo` ownership is `mavenrepo:mavenrepo` and the binary at `/opt/mavenrepo/bin/mavenrepo` is executable |
| nginx 502/503 | `mavenrepo` isn't listening on `127.0.0.1:8080` — check `systemctl status mavenrepo`; confirm `-addr` in the unit matches nginx's `upstream` block |
| Newly published artifact 404s | `-regen` may not have run, or ran against the wrong path — re-check the `mvn_group_id`/`mvn_artifact_id`/`mvn_version` passed to `publish-artifact.yml` against the actual directory under `mavenrepo_data_dir` |
| `<latest>`/`<release>` not reflecting a new version | Same as above — metadata only updates when `-regen` runs; direct file placement without it leaves metadata stale |
| nginx config test fails after a manual edit | `nginx -t` on the host for the specific error; manual edits to `/etc/nginx/conf.d/mavenrepo.conf` will be overwritten by the next `ansible-playbook playbook.yml` run regardless — fix it in `templates/mavenrepo.conf.j2` instead |

Useful commands on the host:

```bash
journalctl -u mavenrepo -f          # tail mavenrepo logs
journalctl -u nginx -f              # tail nginx logs
systemctl status mavenrepo nginx cert-renew.timer
nginx -t                            # validate nginx config
ls -la /var/lib/mavenrepo/repo/...  # inspect the repo tree directly
```

## 9. Access model

- **Consumers** (developers, CI/CD, anything running `mvn`/`gradle`): read
  (`GET`/`HEAD`) only, anonymous by default (`mavenrepo_read_auth: false`),
  over HTTPS.
- **Publishers**: only the operator(s) with SSH/sudo access to the Ansible
  control node and this host. No separate application-level write
  credential exists in the current (read-only) configuration.
- **Ansible control node**: needs SSH + sudo to `mvn-dev-repo.slab.com`.
  Treat access to this control node as equivalent to write access to the
  repository.
