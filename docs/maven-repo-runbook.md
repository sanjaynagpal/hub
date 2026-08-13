# Runbook: Hosting a Maven Repository with Jetty, plus Nexus Repository (Community Edition)

_Last updated: 2026-08-13_

This runbook covers two things:

1. Setting up **Jetty** as a simple Maven repository (both read-only consumption and write/deploy support).
2. Installing **Sonatype Nexus Repository Community Edition** — a maintained, purpose-built repository manager — and pointing Maven at it.

---

## Part 1 — Jetty as a Maven repository

### What you're actually building

A Maven repository is nothing more than a directory tree of artifacts laid out in Maven's standard path convention, served over HTTP:

```
<repo-root>/
  com/example/mylib/1.0.0/mylib-1.0.0.jar
  com/example/mylib/1.0.0/mylib-1.0.0.pom
  com/example/mylib/1.0.0/mylib-1.0.0.jar.sha1
  com/example/mylib/1.0.0/maven-metadata.xml
  ...
```

- **Consuming** a repo (`mvn` downloading dependencies) only needs HTTP `GET`. Jetty does this out of the box.
- **Deploying** to a repo (`mvn deploy`) needs HTTP `PUT`. Jetty's `DefaultServlet` does **not** accept `PUT` by default — you enable it with `PutFilter` (below).

> Reality check: Jetty gives you the HTTP transport, but **not** repository-manager features (metadata regeneration, snapshot timestamping, proxy/caching of Maven Central, a UI, fine-grained security). For a fixed set of artifacts this is fine; for a shared repo that many people deploy to, use a real manager (Part 2).

---

### Prerequisites

- Java 17+ installed (Jetty 12 requires Java 17; Jetty 11 requires Java 11).
- A host/VM you control, with a directory to hold the repo (e.g. `/opt/maven-repo`).
- Network access on your chosen port (e.g. 8080, or 443 behind a reverse proxy).

---

### Choosing an implementation for a single-binary repo

For a *small* repo the real requirement is just "an HTTP server that does `GET` with directory listing and accepts `PUT`." That does **not** require Jetty or a servlet container at all. Three options, easiest first:

- **Approach A — a single static Go binary (recommended).** No JVM to pre-install, ~8 MB self-contained executable, cross-compiles to any OS/arch, tiny memory footprint. Best when you just want to drop one file on a box and run it.
- **Approach B — embedded Jetty (Java).** Choose this only if you're already a JVM shop, want to reuse existing Java tooling/auth, or expect to grow into servlet features. Requires a JRE on the host.
- **Approach C — standalone Jetty distribution.** No code to compile, config-file driven; heavier, also needs a JRE.

Jetty earns its keep as a repository front end mainly when you're *already* running on the JVM. If the only goal is "single executable, no runtime dependency," Go (or Rust) is the better fit — which is exactly what Approach A is.

---

### Approach A — Single static Go binary (no JVM required)

One `main.go` (~120 lines, standard library only — no third-party dependencies) that serves the repo for `GET`/`HEAD` with directory listing, accepts `PUT` for `mvn deploy`, supports `DELETE`, and gates writes behind HTTP Basic auth while leaving reads anonymous. It compiles to a single static binary you can `scp` to any server — nothing else needs to be installed.

> This program has been compiled and tested (deploy with auth → 201, download anonymously → 200, unauthenticated write → 401, directory listing works, and path-traversal attempts are contained to the repo root).

**Step 1. Create the repo directory**

```bash
sudo mkdir -p /opt/maven-repo
sudo chown "$USER" /opt/maven-repo
```

**Step 2. `main.go`**

```go
// mavenrepo — a tiny single-binary Maven repository server (Go stdlib only).
//
//	GET/HEAD: download artifacts, with directory listing (repo consumption)
//	PUT:      accept artifact uploads, creating parent dirs (mvn deploy)
//	DELETE:   remove an artifact
//
// Writes (PUT/DELETE) can require HTTP Basic auth while reads stay anonymous.
//
// Snapshot-aware metadata: the server is authoritative for maven-metadata.xml.
// After each artifact upload it regenerates the affected metadata from the files
// actually present on disk (version-level snapshot metadata and the artifact-level
// versions list), writes matching .md5/.sha1 checksums, and serializes writes with
// a mutex so concurrent deploys can't race on the build number. Client-uploaded
// maven-metadata.xml(.md5/.sha1) is ignored in favour of the server's copy.
//
// Build:  go build -o mavenrepo .
// Run:    ./mavenrepo -addr :8080 -root /opt/maven-repo -user deployer -pass s3cret
package main

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"encoding/xml"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	repoRoot string
	mu       sync.Mutex // serializes writes + metadata regeneration
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	root := flag.String("root", "./repo", "repository root directory")
	user := flag.String("user", "", "username required for writes (empty = writes open)")
	pass := flag.String("pass", "", "password required for writes")
	readAuth := flag.Bool("read-auth", false, "also require auth for reads (GET/HEAD)")
	flag.Parse()

	abs, err := filepath.Abs(*root)
	if err != nil {
		log.Fatalf("resolving root: %v", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		log.Fatalf("creating root: %v", err)
	}
	repoRoot = abs

	fileServer := http.FileServer(http.Dir(repoRoot))

	handler := func(w http.ResponseWriter, r *http.Request) {
		isWrite := r.Method == http.MethodPut || r.Method == http.MethodDelete
		if (isWrite || *readAuth) && !authOK(r, *user, *pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="maven"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			fileServer.ServeHTTP(w, r)
		case http.MethodPut:
			handlePut(w, r)
		case http.MethodDelete:
			handleDelete(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}

	log.Printf("maven repo serving %s on %s (snapshot-aware metadata enabled)", repoRoot, *addr)
	log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(handler)))
}

// safePath maps a request path to an absolute path guaranteed to stay under root.
func safePath(urlPath string) (string, bool) {
	clean := filepath.Clean("/" + strings.TrimPrefix(urlPath, "/"))
	full := filepath.Join(repoRoot, clean)
	if full != repoRoot && !strings.HasPrefix(full, repoRoot+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

func handlePut(w http.ResponseWriter, r *http.Request) {
	full, ok := safePath(r.URL.Path)
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	dir := filepath.Dir(full)
	base := filepath.Base(full)

	mu.Lock()
	defer mu.Unlock()

	// The server owns maven-metadata.xml. Ignore client uploads of it (and its
	// checksums); regenerate from disk instead so metadata is always consistent.
	if isMetadataName(base) {
		regenerateForDir(dir)
		w.WriteHeader(http.StatusCreated)
		return
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.Create(full)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(f, r.Body); err != nil {
		f.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f.Close()

	// Regenerate metadata for the version dir (snapshot) and the artifact dir.
	if !isChecksumOrSig(base) {
		regenerateForArtifact(full)
	}
	w.WriteHeader(http.StatusCreated)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	full, ok := safePath(r.URL.Path)
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if err := os.Remove(full); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func authOK(r *http.Request, user, pass string) bool {
	if user == "" {
		return true
	}
	u, p, ok := r.BasicAuth()
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1 &&
		subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
}

// ---- metadata regeneration -------------------------------------------------

func isMetadataName(name string) bool {
	return name == "maven-metadata.xml" || strings.HasPrefix(name, "maven-metadata.xml.")
}

func isChecksumOrSig(name string) bool {
	for _, ext := range []string{".md5", ".sha1", ".sha256", ".sha512", ".asc"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// regenerateForArtifact is called after an artifact file lands. The file's parent
// is always the version dir, so refresh the version-level snapshot metadata (when
// that dir is a -SNAPSHOT) and always the artifact-level versions metadata one
// level up.
func regenerateForArtifact(artifactFile string) {
	verDir := filepath.Dir(artifactFile)
	if strings.HasSuffix(filepath.Base(verDir), "-SNAPSHOT") {
		regenSnapshot(verDir)
	}
	regenArtifact(filepath.Dir(verDir))
}

// regenerateForDir refreshes metadata appropriate to dir, used when a client tries
// to upload maven-metadata.xml (which only ever lives at the snapshot-version level
// or the artifact level): snapshot metadata when dir is a -SNAPSHOT version dir,
// plus the artifact-level metadata one level up; otherwise the artifact-level
// metadata for dir itself.
func regenerateForDir(dir string) {
	if strings.HasSuffix(filepath.Base(dir), "-SNAPSHOT") {
		regenSnapshot(dir)
		regenArtifact(filepath.Dir(dir))
		return
	}
	regenArtifact(dir)
}

// coordsForVersionDir returns groupId, artifactId, version for a version dir.
func coordsForVersionDir(dir string) (group, artifact, version string, ok bool) {
	rel, err := filepath.Rel(repoRoot, dir)
	if err != nil {
		return "", "", "", false
	}
	segs := strings.Split(filepath.ToSlash(rel), "/")
	if len(segs) < 3 { // need at least group/artifact/version
		return "", "", "", false
	}
	version = segs[len(segs)-1]
	artifact = segs[len(segs)-2]
	group = strings.Join(segs[:len(segs)-2], ".")
	return group, artifact, version, group != "" && artifact != ""
}

// coordsForArtifactDir returns groupId, artifactId for an artifact dir.
func coordsForArtifactDir(dir string) (group, artifact string, ok bool) {
	rel, err := filepath.Rel(repoRoot, dir)
	if err != nil {
		return "", "", false
	}
	segs := strings.Split(filepath.ToSlash(rel), "/")
	if len(segs) < 2 { // need at least group/artifact
		return "", "", false
	}
	artifact = segs[len(segs)-1]
	group = strings.Join(segs[:len(segs)-1], ".")
	return group, artifact, group != "" && artifact != ""
}

type snapEntry struct {
	ts         string // yyyyMMdd.HHmmss
	build      int
	classifier string
	ext        string
	value      string // baseVersion-ts-build
}

func regenSnapshot(verDir string) {
	group, artifact, version, ok := coordsForVersionDir(verDir)
	if !ok || !strings.HasSuffix(version, "-SNAPSHOT") {
		return
	}
	baseVersion := strings.TrimSuffix(version, "-SNAPSHOT")

	names, err := readFileNames(verDir)
	if err != nil {
		return
	}

	// Timestamped (unique-version) artifacts: <artifact>-<base>-<ts>-<build>[-<cls>].<ext>
	tsRe, err := regexp.Compile("^" + regexp.QuoteMeta(artifact+"-"+baseVersion+"-") +
		`(\d{8}\.\d{6})-(\d+)(?:-([^.]+))?\.(.+)$`)
	if err != nil {
		return
	}
	// Literal snapshots: <artifact>-<base>-SNAPSHOT[-<cls>].<ext>
	litRe, err := regexp.Compile("^" + regexp.QuoteMeta(artifact+"-"+baseVersion+"-SNAPSHOT") +
		`(?:-([^.]+))?\.(.+)$`)
	if err != nil {
		return
	}

	var entries []snapEntry
	var literal []snapEntry
	for _, n := range names {
		if isMetadataName(n) || isChecksumOrSig(n) {
			continue
		}
		if m := tsRe.FindStringSubmatch(n); m != nil {
			b, _ := strconv.Atoi(m[2])
			entries = append(entries, snapEntry{
				ts: m[1], build: b, classifier: m[3], ext: m[4],
				value: baseVersion + "-" + m[1] + "-" + m[2],
			})
			continue
		}
		if m := litRe.FindStringSubmatch(n); m != nil {
			literal = append(literal, snapEntry{
				classifier: m[1], ext: m[2], value: version,
			})
		}
	}

	md := &metadata{ModelVersion: "1.1.0", GroupID: group, ArtifactID: artifact, Version: version}

	switch {
	case len(entries) > 0:
		// Find the newest timestamp/build.
		latestTs, latestBuild := "", -1
		for _, e := range entries {
			if e.ts > latestTs || (e.ts == latestTs && e.build > latestBuild) {
				latestTs, latestBuild = e.ts, e.build
			}
		}
		updated := strings.Replace(latestTs, ".", "", 1) // yyyyMMddHHmmss
		md.Versioning.Snapshot = &snapshot{Timestamp: latestTs, BuildNumber: latestBuild}
		md.Versioning.LastUpdated = updated
		seen := map[string]bool{}
		var svs []snapshotVersion
		for _, e := range entries {
			if e.ts != latestTs || e.build != latestBuild {
				continue
			}
			key := e.classifier + "|" + e.ext
			if seen[key] {
				continue
			}
			seen[key] = true
			svs = append(svs, snapshotVersion{
				Classifier: e.classifier, Extension: e.ext, Value: e.value, Updated: updated,
			})
		}
		sortSnapshotVersions(svs)
		md.Versioning.SnapshotVersions = &snapVersionsList{SnapshotVersion: svs}
	case len(literal) > 0:
		updated := nowStamp()
		md.Versioning.Snapshot = &snapshot{LocalCopy: true}
		md.Versioning.LastUpdated = updated
		seen := map[string]bool{}
		var svs []snapshotVersion
		for _, e := range literal {
			key := e.classifier + "|" + e.ext
			if seen[key] {
				continue
			}
			seen[key] = true
			svs = append(svs, snapshotVersion{
				Classifier: e.classifier, Extension: e.ext, Value: e.value, Updated: updated,
			})
		}
		sortSnapshotVersions(svs)
		md.Versioning.SnapshotVersions = &snapVersionsList{SnapshotVersion: svs}
	default:
		return // no artifacts to describe
	}

	writeMeta(verDir, md)
}

func regenArtifact(artifactDir string) {
	group, artifact, ok := coordsForArtifactDir(artifactDir)
	if !ok {
		return
	}
	ents, err := os.ReadDir(artifactDir)
	if err != nil {
		return
	}
	var versions []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if dirHasArtifact(filepath.Join(artifactDir, e.Name())) {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		return
	}
	sort.Slice(versions, func(i, j int) bool { return compareVersion(versions[i], versions[j]) < 0 })

	latest := versions[len(versions)-1]
	release := ""
	for i := len(versions) - 1; i >= 0; i-- {
		if !strings.HasSuffix(versions[i], "-SNAPSHOT") {
			release = versions[i]
			break
		}
	}

	md := &metadata{GroupID: group, ArtifactID: artifact}
	md.Versioning.Latest = latest
	md.Versioning.Release = release
	md.Versioning.Versions = &versionsList{Version: versions}
	md.Versioning.LastUpdated = nowStamp()
	writeMeta(artifactDir, md)
}

func dirHasArtifact(dir string) bool {
	names, err := readFileNames(dir)
	if err != nil {
		return false
	}
	for _, n := range names {
		if isMetadataName(n) || isChecksumOrSig(n) {
			continue
		}
		return true
	}
	return false
}

func readFileNames(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func writeMeta(dir string, md *metadata) {
	out, err := xml.MarshalIndent(md, "", "  ")
	if err != nil {
		return
	}
	data := []byte(xml.Header + string(out) + "\n")
	path := filepath.Join(dir, "maven-metadata.xml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return
	}
	writeChecksums(path, data)
}

func writeChecksums(path string, data []byte) {
	m := md5.Sum(data)
	s := sha1.Sum(data)
	_ = os.WriteFile(path+".md5", []byte(hex.EncodeToString(m[:])), 0o644)
	_ = os.WriteFile(path+".sha1", []byte(hex.EncodeToString(s[:])), 0o644)
}

func nowStamp() string { return time.Now().UTC().Format("20060102150405") }

func sortSnapshotVersions(svs []snapshotVersion) {
	sort.Slice(svs, func(i, j int) bool {
		if svs[i].Extension != svs[j].Extension {
			return svs[i].Extension < svs[j].Extension
		}
		return svs[i].Classifier < svs[j].Classifier
	})
}

// compareVersion is a best-effort Maven-ish version comparison. Numeric tokens
// compare numerically; a trailing qualifier (e.g. -SNAPSHOT) sorts before the
// corresponding release. Good enough for latest/release selection.
func compareVersion(a, b string) int {
	ta, tb := tokenize(a), tokenize(b)
	for i := 0; i < len(ta) || i < len(tb); i++ {
		if i >= len(ta) {
			if isQualifier(tb[i]) {
				return 1 // a (shorter, release) is greater
			}
			return -1
		}
		if i >= len(tb) {
			if isQualifier(ta[i]) {
				return -1
			}
			return 1
		}
		na, ea := strconv.Atoi(ta[i])
		nb, eb := strconv.Atoi(tb[i])
		if ea == nil && eb == nil {
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
			continue
		}
		if ta[i] != tb[i] {
			if ta[i] < tb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func tokenize(v string) []string {
	return strings.FieldsFunc(strings.ToLower(v), func(r rune) bool {
		return r == '.' || r == '-' || r == '_'
	})
}

func isQualifier(tok string) bool {
	switch tok {
	case "snapshot", "alpha", "beta", "rc", "cr", "milestone", "m", "pre":
		return true
	}
	return false
}

// ---- maven-metadata.xml model ----------------------------------------------

type metadata struct {
	XMLName      xml.Name   `xml:"metadata"`
	ModelVersion string     `xml:"modelVersion,attr,omitempty"`
	GroupID      string     `xml:"groupId"`
	ArtifactID   string     `xml:"artifactId"`
	Version      string     `xml:"version,omitempty"`
	Versioning   versioning `xml:"versioning"`
}

type versioning struct {
	Latest           string            `xml:"latest,omitempty"`
	Release          string            `xml:"release,omitempty"`
	Snapshot         *snapshot         `xml:"snapshot,omitempty"`
	Versions         *versionsList     `xml:"versions,omitempty"`
	LastUpdated      string            `xml:"lastUpdated,omitempty"`
	SnapshotVersions *snapVersionsList `xml:"snapshotVersions,omitempty"`
}

type snapshot struct {
	Timestamp   string `xml:"timestamp,omitempty"`
	BuildNumber int    `xml:"buildNumber,omitempty"`
	LocalCopy   bool   `xml:"localCopy,omitempty"`
}

type versionsList struct {
	Version []string `xml:"version"`
}

type snapVersionsList struct {
	SnapshotVersion []snapshotVersion `xml:"snapshotVersion"`
}

type snapshotVersion struct {
	Classifier string `xml:"classifier,omitempty"`
	Extension  string `xml:"extension"`
	Value      string `xml:"value"`
	Updated    string `xml:"updated"`
}
```

**Step 3. Build and run**

```bash
go mod init mavenrepo
go build -o mavenrepo .

# Anonymous read, authenticated write:
./mavenrepo -addr :8080 -root /opt/maven-repo -user deployer -pass s3cret
```

Cross-compile a binary for a different server without any toolchain on the target:

```bash
GOOS=linux   GOARCH=amd64 go build -o mavenrepo-linux-amd64 .
GOOS=linux   GOARCH=arm64 go build -o mavenrepo-linux-arm64 .
GOOS=darwin  GOARCH=arm64 go build -o mavenrepo-macos-arm64 .
```

**Step 4. Consume and deploy** — identical to the Jetty setup below (same `<repository>` / `<distributionManagement>` / `<server>` blocks; point them at `http://YOUR_HOST:8080/`). For `mvn deploy` to authenticate, put the `deployer` credentials in a `<server>` entry in `settings.xml` whose `<id>` matches your `distributionManagement` repo id.

**Step 5. Run as a systemd service**

```ini
[Unit]
Description=Maven Repository (Go)
After=network.target

[Service]
User=mavenrepo
ExecStart=/opt/mavenrepo/mavenrepo -addr :8080 -root /opt/maven-repo -user deployer -pass CHANGEME
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

> **Snapshot & metadata handling (built in).** After each artifact upload the server regenerates `maven-metadata.xml` from the files on disk — the `<snapshot>` timestamp / `<buildNumber>` and `<snapshotVersions>` for `-SNAPSHOT` versions, and the `<versions>` / `<latest>` / `<release>` list at the artifact level — writes matching `.md5`/`.sha1` checksums, serializes writes with a mutex so concurrent deploys can't race on the build number, and ignores client-uploaded metadata in favour of its own copy. This is the main thing a dumb PUT server gets wrong.
>
> **Remaining limitations:** it does not proxy/cache Maven Central, has no web UI or fine-grained RBAC, and rebuilds metadata by scanning the version directory on each write (fine for small/medium repos). Put TLS (nginx/Caddy) in front before exposing it — Basic auth over plaintext is not enough. For the rest, jump to Nexus in Part 2.
---

### Approach B — Embedded Jetty (Java / JVM)

This gives you one small Java program that serves the repo and, optionally, accepts deploys. Pinned to **Jetty 11** because its API is stable and well documented; Jetty 12 notes follow. Prefer this over Approach A only if you're already invested in the JVM.

**Step 1. Create the repo directory and lay out artifacts**

```bash
sudo mkdir -p /opt/maven-repo
sudo chown "$USER" /opt/maven-repo
```

If you already have artifacts built locally, you can seed them:

```bash
mvn deploy:deploy-file \
  -Durl=file:///opt/maven-repo \
  -Dfile=target/mylib-1.0.0.jar \
  -DpomFile=pom.xml
```

**Step 2. Create the embedded Jetty server project**

`RepoServer.java`:

```java
import org.eclipse.jetty.server.Server;
import org.eclipse.jetty.servlet.DefaultServlet;
import org.eclipse.jetty.servlet.FilterHolder;
import org.eclipse.jetty.servlet.ServletContextHandler;
import org.eclipse.jetty.servlet.ServletHolder;
import org.eclipse.jetty.servlets.PutFilter;
import org.eclipse.jetty.util.resource.Resource;

import jakarta.servlet.DispatcherType;
import java.util.EnumSet;

public class RepoServer {
    public static void main(String[] args) throws Exception {
        int port = Integer.parseInt(System.getProperty("port", "8080"));
        String repoDir = System.getProperty("repo", "/opt/maven-repo");

        Server server = new Server(port);

        ServletContextHandler ctx = new ServletContextHandler();
        ctx.setContextPath("/");
        ctx.setBaseResource(Resource.newResource(repoDir));

        // Serve GET requests with directory listing enabled.
        ServletHolder def = new ServletHolder("default", DefaultServlet.class);
        def.setInitParameter("dirAllowed", "true");
        def.setInitParameter("acceptRanges", "true");
        ctx.addServlet(def, "/");

        // OPTIONAL: enable HTTP PUT so `mvn deploy` can write artifacts.
        // Remove this block if you want a strictly read-only repo.
        FilterHolder put = new FilterHolder(new PutFilter());
        ctx.addFilter(put, "/*", EnumSet.of(DispatcherType.REQUEST));

        server.setHandler(ctx);
        server.start();
        System.out.println("Maven repo serving " + repoDir + " on :" + port);
        server.join();
    }
}
```

**Step 3. Build it with Maven**

`pom.xml`:

```xml
<project xmlns="http://maven.apache.org/POM/4.0.0"
         xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
         xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 http://maven.apache.org/xsd/maven-4.0.0.xsd">
  <modelVersion>4.0.0</modelVersion>
  <groupId>com.example</groupId>
  <artifactId>jetty-maven-repo</artifactId>
  <version>1.0.0</version>
  <packaging>jar</packaging>

  <properties>
    <jetty.version>11.0.20</jetty.version>
    <maven.compiler.release>17</maven.compiler.release>
  </properties>

  <dependencies>
    <dependency>
      <groupId>org.eclipse.jetty</groupId>
      <artifactId>jetty-server</artifactId>
      <version>${jetty.version}</version>
    </dependency>
    <dependency>
      <groupId>org.eclipse.jetty</groupId>
      <artifactId>jetty-servlet</artifactId>
      <version>${jetty.version}</version>
    </dependency>
    <dependency>
      <groupId>org.eclipse.jetty</groupId>
      <artifactId>jetty-servlets</artifactId>
      <version>${jetty.version}</version>
    </dependency>
  </dependencies>

  <build>
    <plugins>
      <plugin>
        <groupId>org.apache.maven.plugins</groupId>
        <artifactId>maven-shade-plugin</artifactId>
        <version>3.5.1</version>
        <executions>
          <execution>
            <phase>package</phase>
            <goals><goal>shade</goal></goals>
            <configuration>
              <transformers>
                <transformer implementation="org.apache.maven.plugins.shade.resource.ManifestResourceTransformer">
                  <mainClass>RepoServer</mainClass>
                </transformer>
              </transformers>
            </configuration>
          </execution>
        </executions>
      </plugin>
    </plugins>
  </build>
</project>
```

```bash
mvn -q clean package
```

**Step 4. Run it**

```bash
java -Dport=8080 -Drepo=/opt/maven-repo -jar target/jetty-maven-repo-1.0.0.jar
```

**Step 5. Consume the repo from another project**

In the consuming project's `pom.xml` (or `settings.xml`):

```xml
<repositories>
  <repository>
    <id>my-jetty-repo</id>
    <url>http://YOUR_HOST:8080/</url>
    <releases><enabled>true</enabled></releases>
    <snapshots><enabled>true</enabled></snapshots>
  </repository>
</repositories>
```

```bash
mvn dependency:get -Dartifact=com.example:mylib:1.0.0
```

**Step 6. (If PUT enabled) Deploy to the repo over HTTP**

In the publishing project's `pom.xml`:

```xml
<distributionManagement>
  <repository>
    <id>my-jetty-repo</id>
    <url>http://YOUR_HOST:8080/</url>
  </repository>
</distributionManagement>
```

```bash
mvn deploy
```

> Caveat: `PutFilter` writes the file exactly where Maven PUTs it, including checksums and `maven-metadata.xml` that Maven generates client-side. That works for straightforward release deploys. It does **not** do server-side metadata reconciliation, so concurrent deploys or hand-editing can leave metadata inconsistent. This is precisely the gap a repository manager fills.

---

### Approach C — Standalone Jetty distribution (no code to compile)

If you'd rather not build a Java project:

**Step 1. Install Jetty**

```bash
cd /opt
curl -LO https://repo1.maven.org/maven2/org/eclipse/jetty/jetty-home/11.0.20/jetty-home-11.0.20.tar.gz
tar xzf jetty-home-11.0.20.tar.gz
export JETTY_HOME=/opt/jetty-home-11.0.20
```

**Step 2. Create a Jetty base and enable modules**

```bash
mkdir -p /opt/jetty-base && cd /opt/jetty-base
java -jar "$JETTY_HOME/start.jar" --add-modules=http,deploy,servlet
```

**Step 3. Add a context that serves the repo directory**

Create `/opt/jetty-base/webapps/maven-repo.xml`:

```xml
<?xml version="1.0"?>
<!DOCTYPE Configure PUBLIC "-//Jetty//Configure//EN" "https://www.eclipse.org/jetty/configure_10_0.dtd">
<Configure class="org.eclipse.jetty.servlet.ServletContextHandler">
  <Set name="contextPath">/</Set>
  <Set name="baseResource">
    <New class="org.eclipse.jetty.util.resource.PathResource">
      <Arg>/opt/maven-repo</Arg>
    </New>
  </Set>
  <Call name="addServlet">
    <Arg>org.eclipse.jetty.servlet.DefaultServlet</Arg>
    <Arg>/</Arg>
  </Call>
  <Get name="servletHandler">
    <Call name="getServlet">
      <Arg>org.eclipse.jetty.servlet.DefaultServlet</Arg>
    </Call>
  </Get>
</Configure>
```

**Step 4. Start Jetty**

```bash
cd /opt/jetty-base
java -jar "$JETTY_HOME/start.jar"
```

This serves GET requests. To accept deploys, add `PutFilter` (as in Approach A) via the same context XML, or front the directory with a WebDAV module.

> Which Jetty version? **Jetty 12 is the current, actively-maintained major line** — the current release is the 12.1.x series (12.1.12, July 2026). Jetty 11, 10, and 9 have all reached end of community support, so **for anything new you should target Jetty 12**. The examples above pin Jetty 11 only because its API is the simplest to copy-paste; for a real deployment, use 12.
>
> Jetty 12 differences: the servlet classes moved into environment-specific packages (e.g. `org.eclipse.jetty.ee10.servlet.DefaultServlet`, `org.eclipse.jetty.ee10.servlets.PutFilter`) and you enable an `ee10-*` module set (Jakarta EE 10 / `jakarta.servlet`). The concepts are identical; only class/module names change. To port the embedded example (Approach B), bump `<jetty.version>` to `12.1.x`, add the `jetty-ee10-servlet` / `jetty-ee10-servlets` artifacts, and update the `import` and class references to the `ee10` packages. Jetty 12 requires Java 17+. (Approach A, the Go binary, sidesteps all of this — no JVM, no version dance.)

---

### Hardening checklist (before anyone else uses it)

- **Authentication for writes.** Wrap the context in `BASIC`/`FORM` auth with a `HashLoginService` (or LDAP), and require a role for `PUT`. Never expose an open PUT endpoint to the internet.
- **TLS.** Terminate HTTPS at Jetty (`ssl`, `https` modules) or, more commonly, behind nginx/Apache/Caddy. Maven refuses plaintext repos in newer versions unless explicitly allowed.
- **Read/write separation.** Consider anonymous read + authenticated write.
- **Backups.** The repo directory is your source of truth — back it up.
- **Run as a service.** Wrap the `java -jar ...` command in a `systemd` unit so it restarts on boot/crash.

Example minimal `systemd` unit (`/etc/systemd/system/maven-repo.service`):

```ini
[Unit]
Description=Jetty Maven Repository
After=network.target

[Service]
User=jetty
ExecStart=/usr/bin/java -Dport=8080 -Drepo=/opt/maven-repo -jar /opt/jetty-maven-repo/target/jetty-maven-repo-1.0.0.jar
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now maven-repo
```

---

## Part 2 — Sonatype Nexus Repository (Community Edition)

### What it is

Nexus Repository is the de facto standard **build-artifact repository manager** — the same category as JFrog Artifactory. Unlike raw Jetty, it understands Maven repositories natively and is actively maintained. It provides:

- **Hosted** repositories for your own releases/snapshots, with proper authenticated HTTP deploy.
- **Proxy/caching** of remote repos like Maven Central, so builds don't hit the internet every time.
- **Repository groups** — combine hosted + proxied repos behind a single URL.
- Server-side **metadata management, indexing, and search**.
- A **web UI** and **role-based access control** (users, roles, per-repo privileges).
- Many formats beyond Maven (npm, Docker, PyPI, NuGet, etc.) from one server.

### Naming/licensing note (this changed recently)

What used to be called **"Nexus Repository OSS" was replaced by "Nexus Repository Community Edition" (CE)** starting in version **3.77.0 (February 2025)**. CE is still free, but the free tier now carries **usage limits**, enforced after a 45-day grace period. As of version 3.87.0 the limits are:

- **40,000 components** (down from an initial 100,000)
- **100,000 requests/day** (down from an initial 200,000)
- Intended for a single instance.

When you exceed a limit, the instance **rejects any request that would store a new component** (reads/downloads keep working); you either clean up unused components or move to a paid edition. For a small team's internal Maven repo these limits are usually generous; for a large monorepo or a busy CI fleet, price out **Nexus Repository Pro** or **JFrog Artifactory**. The latest release line at the time of writing is **3.93.x**.

### How to install Nexus Repository CE

Two easy paths — Docker (fastest) or the OS archive. Nexus requires **Java 17**; the Unix archive bundles its own JRE, and the Docker image includes everything, so you don't have to install Java yourself in either case.

#### Option A — Docker (recommended for a quick, clean setup)

**Step 1. Create a persistent data volume and run the container**

```bash
docker volume create nexus-data

docker run -d \
  --name nexus \
  -p 8081:8081 \
  -v nexus-data:/nexus-data \
  sonatype/nexus3:latest
```

- Nexus serves on **port 8081** by default (note: not 8080).
- First boot takes a minute or two while it initializes `/nexus-data`.

**Step 2. Retrieve the initial admin password**

Nexus generates a random admin password on first start, stored inside the data volume:

```bash
docker exec nexus cat /nexus-data/admin.password
```

**Step 3. First-time setup**

1. Open `http://YOUR_HOST:8081/`.
2. Click **Sign in**, log in as `admin` with the password from Step 2.
3. The setup wizard prompts you to set a new admin password and choose whether to **allow anonymous access** (anonymous read is convenient for a pure consumption repo; disable it if the repo is private).

#### Option B — OS archive (systemd service)

**Step 1. Download and unpack**

```bash
cd /opt
# Grab the current Unix bundle from help.sonatype.com/en/download.html
curl -LO https://download.sonatype.com/nexus/3/latest-unix.tar.gz
tar xzf latest-unix.tar.gz
# This creates two dirs: nexus-<version>/ (app) and sonatype-work/ (data)
sudo useradd -r -m nexus
sudo chown -R nexus:nexus /opt/nexus-* /opt/sonatype-work
```

**Step 2. Run it under systemd**

Create `/etc/systemd/system/nexus.service` (adjust the versioned path):

```ini
[Unit]
Description=Sonatype Nexus Repository
After=network.target

[Service]
Type=forking
User=nexus
ExecStart=/opt/nexus-3.93.0-01/bin/nexus start
ExecStop=/opt/nexus-3.93.0-01/bin/nexus stop
Restart=on-failure
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now nexus
```

**Step 3.** Same as Docker: get the initial password from `/opt/sonatype-work/nexus3/admin.password`, then finish the wizard at `http://YOUR_HOST:8081/`.

### Point Maven at Nexus

Out of the box, Nexus creates three Maven repositories you'll use:

- `maven-releases` — hosted, for your release artifacts.
- `maven-snapshots` — hosted, for your `-SNAPSHOT` artifacts.
- `maven-central` — a proxy of Maven Central.
- `maven-public` — a **group** that combines the above behind one URL (use this for *consuming*).

**Consume** — in `~/.m2/settings.xml`, mirror everything through the public group so Central is cached by Nexus:

```xml
<settings>
  <mirrors>
    <mirror>
      <id>nexus-public</id>
      <name>Nexus Public Group</name>
      <url>http://YOUR_HOST:8081/repository/maven-public/</url>
      <mirrorOf>*</mirrorOf>
    </mirror>
  </mirrors>
</settings>
```

**Deploy** — in your project `pom.xml`:

```xml
<distributionManagement>
  <repository>
    <id>nexus-releases</id>
    <url>http://YOUR_HOST:8081/repository/maven-releases/</url>
  </repository>
  <snapshotRepository>
    <id>nexus-snapshots</id>
    <url>http://YOUR_HOST:8081/repository/maven-snapshots/</url>
  </snapshotRepository>
</distributionManagement>
```

Add matching credentials in `settings.xml`:

```xml
<servers>
  <server>
    <id>nexus-releases</id>
    <username>deployer</username>
    <password>••••••</password>
  </server>
  <server>
    <id>nexus-snapshots</id>
    <username>deployer</username>
    <password>••••••</password>
  </server>
</servers>
```

In the Nexus UI, create a `deployer` user (or role) with the `nx-repository-view-maven2-*-add` / `edit` privileges on the target repos, then:

```bash
mvn deploy
```

### Production hardening checklist

- **Put TLS in front of it.** Run nginx/Caddy/Apache as a reverse proxy terminating HTTPS and forwarding to `:8081`; set Nexus's Base URL accordingly under **Administration → System → Capabilities**. Maven refuses plaintext repos in newer versions.
- **Disable anonymous access** for private repos; grant read via a dedicated role instead.
- **Back up `nexus-data`** (the volume / `sonatype-work` dir) — it holds the blob store and config.
- **Use an external PostgreSQL** database instead of the embedded H2 for anything beyond a small instance (better resiliency; required for HA).
- **Set up a cleanup policy** on snapshots so you don't blow through the CE component limit.
- **Give the JVM enough heap** — tune `-Xms`/`-Xmx` in `nexus.vmoptions` for your workload.

---

## Summary / recommendation

| Need | Best option |
|---|---|
| Publish a small, fixed set of artifacts, read-only | **Single Go binary** (Approach A) — no JVM, one file; or embedded/standalone Jetty (B/C) if you're a JVM shop |
| Occasional internal deploys, no strong security needs | Go binary with `-user/-pass` (Approach A), or Jetty + `PutFilter` + BASIC auth |
| Shared team repo: deploys, proxy/cache Central, search, RBAC, UI | **Nexus Repository CE** (recommended) or JFrog Artifactory |
| Large monorepo / busy CI that exceeds CE's 40k-component or 100k-request/day limits | **Nexus Pro** or **Artifactory** (paid) |

Jetty is a fine transport layer for a repository but is **not** a repository manager. Nexus Repository *is* one and is actively maintained — so if you're standing something up today that a team will rely on, Nexus CE is the right landing spot, as long as your volume fits inside the free tier's limits.
