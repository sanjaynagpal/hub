# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`hub` (module `github.com/sanjaynagpal/hub`) is a Maven repository server
written in Go, standard library only — no third-party dependencies. The
standalone/hub-mode server lives in `main.go` plus `main_test.go`; Hub Proxy
mode (a read-through cache in front of another Hub — see "Hub Proxy mode"
below) is a second, self-contained concern kept in a sibling file,
`proxy.go` plus `proxy_test.go`, same package `main`, same binary. Read
these directly rather than hunting through an `internal/`/`pkg/` layout,
since there isn't one.

The one other Go package in this repo is `cmd/acmeclient/` — a small,
independent, stdlib-only ACME (RFC 8555) client that provisions the TLS
certificate for the nginx front described below (`main.go` itself has no TLS
of its own; it's meant to sit behind a reverse proxy). It doesn't import or
get imported by the server; treat it as its own single-file tool with its
own `main_test.go`. It's expiry-aware by default, not a blind
reissue-every-run tool: `run()` reads the installed certificate's actual
`NotAfter` and only renews within `-renew-before-days`, firing an optional
`-notify-cmd` hook both as a heads-up (`-notify-before-days` out) and after
each renewal attempt succeeds or fails. `-force` opts back into unconditional
renewal on every run, for operators who'd rather lean on the CA's own rate
limits — see `docs/acme-protocol.md` §9 for the reasoning.

## Commands

```bash
go build -o mavenrepo .        # build
go run . -addr :8080 -root ./repo   # run locally
go test ./...                  # run all tests (both packages)
go test -run TestSnapshotMetadataRegeneration ./...   # run a single test
go vet ./...
```

Building `cmd/acmeclient` (only needed when working on TLS provisioning, not
the server itself):

```bash
go build -o acmeclient ./cmd/acmeclient
```

Cross-compiling (no target-host toolchain needed):

```bash
GOOS=linux GOARCH=amd64 go build -o mavenrepo-linux-amd64 .
GOOS=linux GOARCH=arm64 go build -o mavenrepo-linux-arm64 .
GOOS=linux GOARCH=amd64 go build -o acmeclient-linux-amd64 ./cmd/acmeclient
```

Docker (multi-stage, static binary into `FROM scratch`, non-root uid 65532):

```bash
docker build -t hub .
docker run -d --name hub -p 8080:8080 -v hub-repo:/repo hub -user deployer -pass s3cret
```

## Architecture

The whole server is one `http.HandlerFunc` (`newHandler` in main.go) dispatching
on method:

- `GET`/`HEAD` → delegated straight to `http.FileServer(http.Dir(repoRoot))`
  (directory listing comes for free from the stdlib).
- `PUT` → `handlePut`: writes the uploaded file, then triggers metadata
  regeneration.
- `DELETE` → `handleDelete`: removes a file.

Auth (`authOK`) is HTTP Basic, checked only for writes unless `-read-auth` is
set; comparisons use `crypto/subtle` to stay constant-time. All writes go
through a single package-level `mu sync.Mutex` (serializes writes and metadata
regeneration so concurrent `mvn deploy`s can't race the snapshot build number).

Per-request access logging (`-access-log`, on by default) is `accessLogMiddleware`
wrapping the handler once in `main()`, not part of `newHandler`/`newProxyHandler`
themselves -- both apply it identically, and tests that construct a handler
directly (most of `main_test.go`/`proxy_test.go`) exercise the unwrapped handler,
so they see no access-log output. Its `statusWriter` forwards `Flush()` to the
underlying `http.ResponseWriter`, which the long-lived `GET /-/hub/events`
stream depends on.

**Path safety**: every request path is resolved through `safePath`, which
`filepath.Clean`s and joins against `repoRoot`, then verifies the result is
still under `repoRoot` before any file I/O — this is the sole path-traversal
guard and any new handler touching the filesystem must go through it.

Two flags change the write path's shape: `-read-only` rejects `PUT`/`DELETE`
outright (405) regardless of credentials, for repos populated out-of-band
instead of via `mvn deploy`; `-regen <version-dir>` runs metadata
regeneration for one version directory and exits without starting the
server, for artifacts placed on disk directly (the read-only counterpart to
a normal `PUT` triggering regeneration). Both are covered in
`docs/operator-runbook.md`.

`safePath(root, urlPath string) (string, bool)` takes an explicit root
rather than reading the `repoRoot` package global — needed so a Hub Proxy's
own cache root and a standalone Hub's `repoRoot` can coexist in the same
process (chiefly for tests that run both in one binary). `newHandler` takes
a 5th `allowProxies bool` param; when true it serves `GET /-/hub/events` (a
long-lived NDJSON stream of change notifications, see below) ahead of the
normal file-serving path.

### Hub Proxy mode (`proxy.go`)

`-upstream <url>` switches the binary into a **Hub Proxy**: a read-through
cache in front of another Hub. It serves `GET`/`HEAD` from its own
`-root` when cached, otherwise fetches-and-caches from `-upstream` (a
hand-rolled singleflight collapses concurrent misses of the same path into
one upstream fetch — no third-party dependency); `PUT`/`DELETE` always 405.
It must complete an initial connection to the upstream's
`GET /-/hub/events` (gated by `-allow-proxies` on the upstream) within
`-join-timeout` before it starts serving at all (`main()` `log.Fatal`s
otherwise) — but a later disconnect degrades to serving possibly-stale
cached content, not failing requests. Staleness is bounded two ways: the
event stream invalidates affected cache entries the instant the upstream
changes (mirroring `regenerateForVersionDir`'s own "walk up" shape as
deletion instead of regeneration), and a `-sync-interval` poll fallback
re-validates cached `maven-metadata.xml` files only while that stream is
down. See the README's "Proxy mode" section for the full flag list.

One Windows-specific wrinkle worth knowing if touching `proxy.go`:
`invalidate`'s `os.RemoveAll` and the cache-fill `os.Rename` both retry
briefly (`removeRetry`/`renameRetry`) against a transient sharing-violation
error — something else (real-time AV scanning is the common cause) can
momentarily hold a handle on a just-served cache file exactly when
invalidation or a rename tries to touch it. This was an actual observed
source of test flakiness on this repo's native-Windows dev loop, not
theoretical; don't remove the retry as dead code.

**Mirror mode** (`-mirror`, requires `-upstream`) is a `proxyState` with
`mirror: true` — it reuses `newProxyHandler`/`serveOrFetch`/
`fetchAndCache`/`invalidate`/the event loop completely unchanged (a
mirror's cache-miss and event-invalidation behavior is identical to a
plain proxy's). What it adds: `startBackgroundSync` additionally launches
`runFullSync` (one eager `reconcileWithManifest` pass right after join) and
`runMirrorPollFallback` (the same ticker/`connected`-gated shape as
`runPollFallback`, but calling `reconcileWithManifest` instead of
`pollOnce`). `reconcileWithManifest` fetches the hub-side
`GET /-/hub/manifest` endpoint (`handleManifest` in main.go, gated by
`-allow-proxies` exactly like `/-/hub/events` — an NDJSON listing of every
regular file under `repoRoot`, metadata/checksums included) and reconciles:
fetches anything missing locally (bounded by `-mirror-concurrency`), and
`removeRetry`s anything local no longer in the manifest (catches an
upstream deletion that happened during a disconnect). This is the only
mechanism by which a mirror discovers brand-new upstream artifacts it was
never asked for — a plain proxy's `pollOnce` can't, since it only
re-validates paths already cached.

### Metadata regeneration (the core logic)

The server treats itself as authoritative for `maven-metadata.xml` — client
uploads of that file (and its checksums) are silently discarded and
regenerated from what's actually on disk instead (`isMetadataName` check in
`handlePut`). This is the main non-obvious piece of behavior in the codebase:

- After any artifact `PUT`, `regenerateForArtifact` walks up from the file:
  the immediate parent is always a version directory. If that version ends in
  `-SNAPSHOT`, `regenSnapshot` rebuilds the version-level metadata (`<snapshot>`
  timestamp/buildNumber and `<snapshotVersions>` list); `regenArtifact` always
  runs one level up to rebuild the artifact-level `<versions>`/`<latest>`/`<release>`.
- `regenSnapshot` recognizes two on-disk naming schemes for snapshot artifacts
  via two regexes: the standard timestamped/unique-version form
  (`artifact-base-yyyyMMdd.HHmmss-build[-classifier].ext`) and literal
  `-SNAPSHOT` filenames. It picks whichever files match the *newest*
  timestamp+build, dedupes by classifier+extension, and writes matching
  `<snapshotVersion>` entries.
- `regenArtifact` lists version subdirectories (via `dirHasArtifact`, which
  ignores metadata/checksum files so empty-of-real-content dirs don't count),
  orders them with `compareVersion` (a Maven-ish numeric/qualifier version
  comparator — `-SNAPSHOT` sorts before its release), and picks `latest`
  (last in order) and `release` (last non-`-SNAPSHOT`).
- Every metadata write goes through `writeMeta`, which also writes matching
  `.md5`/`.sha1` sidecars (`writeChecksums`) so checksum-verifying clients
  stay happy.

When modifying metadata behavior, the four functions to keep in sync are
`regenerateForArtifact`/`regenerateForDir` (when regeneration is triggered)
and `regenSnapshot`/`regenArtifact` (what gets written) — tests in
`main_test.go` cover both the unique-version and literal-`-SNAPSHOT` layouts,
build-number advancement, artifact-level `latest`/`release` selection, and
checksum consistency, so run `go test ./...` after any change here.

## Docs

- `docs/maven-repo-runbook.md` — broader runbook comparing this Go server
  against Jetty and Sonatype Nexus Repository as alternatives for hosting a
  Maven repo — useful background if asked about scope/limitations or
  alternative approaches.
- `docs/architecture.md` — architecture doc for the `mvn-dev-repo.slab.com`
  deployment specifically: component breakdown, the `-read-only` +
  operator-publish design decision and why `mvn deploy` credentials were
  rejected for this host, and open items (confirming the internal CA's ACME
  directory URL/EAB requirement, etc).
- `docs/operator-runbook.md` — task-oriented ops guide for that same
  deployment: first-time setup, publishing/removing an artifact via
  `deploy/ansible/publish-artifact.yml`, cert renewal, troubleshooting.
- `docs/acme-protocol.md` — protocol-level reference for ACME (RFC 8555)
  itself: message format, resources/state machines, challenge types, the
  full issuance sequence — independent of this repo, with pointers into
  `cmd/acmeclient/main.go` for where each piece is implemented.

None of these four are something the code depends on, but `architecture.md`
and `operator-runbook.md` describe `-read-only`/`-regen` (main.go flags) and
the Ansible layout under `deploy/`, and `acme-protocol.md` describes
`cmd/acmeclient`'s behavior, so re-check the relevant doc for accuracy if any
of those change.
