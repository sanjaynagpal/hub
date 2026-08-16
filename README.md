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

**Not implemented, worth knowing about**:
- No built-in request/access logging beyond the startup line and error paths — rely on nginx's access log (or add your own reverse-proxy logging) for a full audit trail of who read/wrote what.
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
+ TLS config (internal-CA REST API renewal instead of ACME/Let's Encrypt).
