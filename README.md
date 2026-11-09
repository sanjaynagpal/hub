# hub — a tiny single-binary Maven repository server

`hub` is a minimal Maven repository server written in Go (standard library only,
no third-party dependencies). It serves a directory tree over HTTP so it can act
as a small Maven repository:

- **GET / HEAD** — download artifacts, with directory listing (repository consumption)
- **PUT** — accept artifact uploads, creating parent directories (`mvn deploy`)
- **DELETE** — remove an artifact

Writes (`PUT`/`DELETE`) can be gated behind HTTP Basic auth while reads stay
anonymous — the common shape for an internal repo. It compiles to a single
static binary with no runtime to pre-install.

## Build

```bash
go build -o mavenrepo .
```

Cross-compile for another server with no toolchain on the target:

```bash
GOOS=linux  GOARCH=amd64 go build -o mavenrepo-linux-amd64 .
GOOS=linux  GOARCH=arm64 go build -o mavenrepo-linux-arm64 .
```

## Run

```bash
# Anonymous read, authenticated write:
./mavenrepo -addr :8080 -root /opt/maven-repo -user deployer -pass s3cret
```

| Flag         | Default   | Meaning                                        |
|--------------|-----------|------------------------------------------------|
| `-addr`      | `:8080`   | Listen address                                 |
| `-root`      | `./repo`  | Repository root directory                      |
| `-user`      | *(empty)* | Username required for writes (empty = open)    |
| `-pass`      | *(empty)* | Password required for writes                   |
| `-pass-file` | *(empty)* | Path to a file holding the password; overrides `-pass` and keeps it out of argv/unit files (e.g. for systemd deployments) |
| `-read-auth` | `false`   | Also require auth for reads (GET/HEAD)         |
| `-read-only` | `false`   | Reject PUT/DELETE outright (405) regardless of credentials — for repos populated out-of-band instead of via `mvn deploy` |
| `-regen`     | *(empty)* | Regenerate `maven-metadata.xml` for the given version directory (relative to `-root`) and exit, without starting the server — for artifacts placed directly on disk |
| `-max-upload-size` | `524288000` (500 MiB) | Maximum PUT request body size in bytes, enforced by the server itself regardless of any reverse-proxy limit (`0` = unlimited) |
| `-allow-proxies` | `false` | Expose `GET /-/hub/events`, a change-notification stream for Hub Proxy instances (see below) |
| `-access-log` | `true` | Log one line per request (remote addr, method, path, status, bytes, duration) — turn off if something already sits in front of Hub and logs access (e.g. nginx) |

## Proxy mode

`hub` can also run as a **Hub Proxy**: a read-through cache in front of another Hub instance (the *upstream*). A proxy serves artifacts from its own local cache when it has them, and otherwise fetches them from the upstream and caches them before replying. It never accepts writes — `PUT`/`DELETE` always return 405, regardless of credentials; `mvn deploy` always targets the upstream directly.

```bash
# On the upstream, allow proxies to attach:
./mavenrepo -addr :8080 -root /opt/maven-repo -allow-proxies

# On the proxy:
./mavenrepo -addr :8080 -root ./proxy-cache -upstream http://upstream-host:8080
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-upstream` | *(empty)* | Upstream Hub base URL. Presence of this flag switches the binary into proxy mode |
| `-upstream-user` / `-upstream-pass` / `-upstream-pass-file` | *(empty)* | Credentials for authenticating *to* the upstream — independent of this proxy's own `-user`/`-pass`/`-read-auth`, which still gate its own downstream clients |
| `-sync-interval` | `60s` | Poll-fallback interval, active only while the push connection to the upstream is disconnected |
| `-join-timeout` | `30s` | How long to wait for the initial connection to the upstream before giving up and exiting |

A proxy must successfully **join** its upstream (connect to `GET /-/hub/events` on a Hub started with `-allow-proxies`) before it starts serving its own traffic — if the upstream is unreachable at startup, the proxy exits rather than serving from an empty, unsynchronized cache. Once running, staleness is bounded two ways: the upstream pushes a change notification the instant an artifact is written or deleted, so a proxy's cache is invalidated (and re-fetched fresh on the next request) almost immediately; and if that connection drops, the proxy falls back to periodically re-validating its cached `maven-metadata.xml` files every `-sync-interval` until the push connection reconnects. A later disconnect degrades to serving (possibly stale) cached content rather than failing requests — only the *initial* join is mandatory.

`-upstream` cannot be combined with `-regen`, `-read-only` (proxy mode is always read-only), or `-allow-proxies` (proxy-of-proxy chaining isn't supported).

## Mirror mode

A **Hub Mirror** is a Hub Proxy that additionally keeps a *complete* copy of the upstream rather than only caching what's been requested. Add `-mirror` to a proxy invocation:

```bash
./mavenrepo -addr :8080 -root ./mirror-store -upstream http://upstream-host:8080 -mirror
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-mirror` | `false` | Requires `-upstream`. Eagerly pull and keep a complete copy of the upstream instead of only caching what's requested |
| `-mirror-concurrency` | `8` | How many files to fetch in parallel during a full sync pass |

A mirror serves traffic exactly like a proxy (same fetch-through-on-miss behavior, same read-only enforcement) but additionally runs a background sync: right after joining, it fetches the upstream's full file listing (`GET /-/hub/manifest`, also gated by `-allow-proxies`) and pulls anything it's missing; while the push connection is down, it periodically repeats that reconciliation — pulling newly-added upstream files and removing local files the upstream no longer has — rather than only re-validating what it already cached (a Hub Proxy's narrower poll fallback). This makes a mirror's disk a genuine, current replica of the upstream, not a request-shaped subset.

**Disaster recovery**: because a mirror's copy is complete, it's a credible thing to promote to a standalone Hub if the real Hub's machine is lost — stop the mirror process, restart the *same binary* against the *same* `-root` without `-upstream` (add `-user`/`-pass` if that host should accept `mvn deploy` writes again). This is a manual, documented recovery procedure, not an automatic failover: there's no replication/consensus behind it, so if you run multiple mirrors, confirm which one has the most recent successful sync before promoting it.

## Docker

The repo ships a multi-stage `Dockerfile` that builds a fully static binary and
copies it into a `FROM scratch` image (no OS, ~6 MB, runs as a non-root numeric
user):

```bash
docker build -t hub .

# Named volume for persistence; add -user/-pass by appending to the command:
docker run -d --name hub -p 8080:8080 -v hub-repo:/repo hub -user deployer -pass s3cret
```

The container listens on `:8080` and stores artifacts in the `/repo` volume. The
`ENTRYPOINT` is the server, so any flags you append to `docker run ... hub` are
passed straight to it.

## Use from Maven

**Consume** — in a project `pom.xml` or `~/.m2/settings.xml`:

```xml
<repositories>
  <repository>
    <id>hub</id>
    <url>http://YOUR_HOST:8080/</url>
    <releases><enabled>true</enabled></releases>
    <snapshots><enabled>true</enabled></snapshots>
  </repository>
</repositories>
```

**Deploy** — in a project `pom.xml`:

```xml
<distributionManagement>
  <repository>
    <id>hub</id>
    <url>http://YOUR_HOST:8080/</url>
  </repository>
  <snapshotRepository>
    <id>hub</id>
    <url>http://YOUR_HOST:8080/</url>
  </snapshotRepository>
</distributionManagement>
```

with matching credentials in `settings.xml`:

```xml
<servers>
  <server>
    <id>hub</id>
    <username>deployer</username>
    <password>s3cret</password>
  </server>
</servers>
```

```bash
mvn deploy
```

## Snapshot & metadata handling

The server is **authoritative** for `maven-metadata.xml`. After each artifact
upload it regenerates the affected metadata from the files actually present on
disk, rather than trusting whatever the client uploads:

- **Snapshot versions** (`…/<version>-SNAPSHOT/maven-metadata.xml`): the
  `<snapshot>` timestamp / `<buildNumber>` and the `<snapshotVersions>` list are
  rebuilt from the timestamped artifacts on disk, so `mvn deploy` of `-SNAPSHOT`
  builds resolves correctly and the build number advances on its own. Both the
  standard unique-version (timestamped) layout and literal `-SNAPSHOT` filenames
  are recognised.
- **Artifact versions** (`…/<artifactId>/maven-metadata.xml`): the `<versions>`
  list plus `<latest>` and `<release>` are rebuilt from the version directories
  present, using a Maven-ish version ordering (so `-SNAPSHOT` sorts before the
  corresponding release).

Matching `.md5` and `.sha1` checksums are written next to each metadata file so
checksum-verifying clients stay happy, writes are serialized with a mutex so
concurrent deploys can't race on the build number, and client-uploaded
`maven-metadata.xml` (and its checksums) are ignored in favour of the server's
regenerated copy.

## Read-only mode (artifacts published out-of-band)

For repos that only ever hold artifacts an operator has vetted and placed
manually — e.g. a third-party download not on Maven Central — rather than
anything `mvn deploy` pushes routinely, run with `-read-only`. PUT/DELETE are
then rejected outright (405) regardless of credentials, not just auth-gated;
GET/HEAD are unaffected. This sidesteps `mvn deploy` credential management
(distribution, rotation, revocation across every developer and CI job)
entirely, which is real overhead not worth paying when writes are infrequent
and operator-driven anyway. The Ansible deployment in
[`deploy/ansible/`](deploy/ansible/) enforces this at nginx too (a
`limit_except GET HEAD { deny all; }` block), so a write never even reaches
the backend regardless of app-level config drift.

Since a read-only server has no HTTP write path, artifacts have to land on
disk some other way (e.g. an Ansible playbook copying files directly — see
[`deploy/ansible/publish-artifact.yml`](deploy/ansible/publish-artifact.yml)
for a working example). That bypasses the PUT handler entirely, so nothing
triggers the metadata regeneration described below — run
`mavenrepo -root <root> -regen <path/to/version/dir>` afterwards to rebuild
that artifact's `maven-metadata.xml` from what's actually on disk. It's a
one-shot CLI command (the server doesn't need to be running), and pairs
naturally with `-read-only`: since HTTP can never write, `-regen` invocations
are the only writer, so there's no race with the server's internal mutex.

## Security hardening

**Path traversal**: fully guarded, at two independent layers. `GET`/`HEAD`
goes through `http.FileServer(http.Dir(repoRoot))`, whose stdlib `http.Dir`
rejects any path containing `..` outright. `PUT`/`DELETE`/`-regen` all
resolve through `safePath()`, which `filepath.Clean`s (eliminating leading
`..` on a rooted path) and joins against `repoRoot`, then verifies the result
is still prefixed by `repoRoot` before any file I/O. Covered by
`TestSafePathContainsTraversal`. Not covered: a symlink placed under
`repoRoot` pointing outside it would be followed on `GET` — not exploitable
through this server's own HTTP interface (`PUT` only ever writes literal file
content, never a symlink), but worth knowing if something else on the host
could plant one.

**Implemented**:
- Constant-time credential comparison (`crypto/subtle`) — no timing side channel on `-user`/`-pass`.
- `-read-only` (see above) plus the nginx-level `limit_except` mirror in `deploy/ansible/`.
- `-pass-file` keeps the write password off argv/`ps`/unit files.
- `-max-upload-size` (default 500 MiB) caps `PUT` body size at the app layer via `http.MaxBytesReader`, independent of any reverse-proxy limit; a request over the cap gets 413 and no partial file is left on disk.
- Explicit `ReadHeaderTimeout`/`IdleTimeout` on the HTTP server, guarding against slowloris-style connections that hold sockets open by trickling headers in slowly. `ReadTimeout`/`WriteTimeout` are deliberately left unset so they don't cap large legitimate artifact transfers.
- Error responses to clients are generic; the real error (which can include server-side file paths) is logged server-side only, not returned in the response body.
- `server_tokens off;` in the shipped nginx configs, so the exact nginx version isn't handed to anyone probing the server.
- `-access-log` (default on) logs one line per request (remote addr, method, path, status, bytes, duration) — a full audit trail of who read/wrote what even when Hub is reached directly, with nothing in front of it. Turn it off if you'd rather rely solely on a reverse proxy's own access log and avoid logging the same request twice.

**Not implemented, worth knowing about**:
- No rate limiting on Basic Auth attempts at the app layer — add `limit_req` at nginx if `-read-only` is off and brute-forcing `-pass` is a real concern for your deployment.
- Directory listing is on by default (`http.FileServer`'s built-in behavior) — expected for a browsable repo, but confirm that's actually wanted for your deployment; there's no flag to disable it.
- No `X-Content-Type-Options: nosniff` — mostly relevant if the general-purpose write mode (`-read-only` off) is used by less-trusted principals, since a served `.html` upload could execute inline script for anyone linked directly to it.

## Scope & limitations

Still intentionally minimal. It does **not** proxy or cache Maven Central, has no
web UI or fine-grained RBAC, and rebuilds metadata by scanning the version
directory on each write (perfectly fine for small/medium repos, but not tuned for
very large ones). For those capabilities use a full repository manager — see
[`docs/maven-repo-runbook.md`](docs/maven-repo-runbook.md), which also covers the
embedded/standalone **Jetty** alternatives and when to reach for **Sonatype Nexus
Repository**. Put TLS in front before exposing it — Basic auth over plaintext is
not enough. [`deploy/nginx/`](deploy/nginx/) has a ready-made nginx reverse-proxy
+ TLS config, renewed from the internal CA over ACME (RFC 8555) via
[`cmd/acmeclient`](cmd/acmeclient/) — a minimal stdlib-only client, in keeping
with the rest of this repo, rather than a Python/certbot dependency. It checks
the installed certificate's actual expiry (works with CAs issuing anything
from day- to month-scale certificates), only renews once close to expiry, and
can notify a hook of your choosing ahead of and after each renewal.
