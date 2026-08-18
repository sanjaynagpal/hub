# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`hub` (module `github.com/sanjaynagpal/hub`) is a single-file Maven repository
server written in Go, standard library only — no third-party dependencies.
Everything the server needs lives in `main.go` (~540 lines) plus
`main_test.go`; read `main.go` directly rather than hunting through an
`internal/`/`pkg/` layout, since there isn't one.

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

**Path safety**: every request path is resolved through `safePath`, which
`filepath.Clean`s and joins against `repoRoot`, then verifies the result is
still under `repoRoot` before any file I/O — this is the sole path-traversal
guard and any new handler touching the filesystem must go through it.

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
