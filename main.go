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
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
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
	repoRoot      string
	mu            sync.Mutex // serializes writes + metadata regeneration
	maxUploadSize int64      // 0 = unlimited; set from -max-upload-size in main()

	// eventsBroadcaster is non-nil only under -allow-proxies. Broadcasting
	// through a nil receiver is a deliberate no-op (see (*eventBroadcaster).broadcast).
	eventsBroadcaster *eventBroadcaster
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	root := flag.String("root", "./repo", "repository root directory")
	user := flag.String("user", "", "username required for writes (empty = writes open)")
	pass := flag.String("pass", "", "password required for writes")
	passFile := flag.String("pass-file", "", "path to a file containing the write password (overrides -pass; keeps the secret out of argv and unit files)")
	readAuth := flag.Bool("read-auth", false, "also require auth for reads (GET/HEAD)")
	readOnly := flag.Bool("read-only", false, "reject PUT/DELETE outright (405), regardless of credentials -- for repos populated out-of-band instead of via mvn deploy")
	regen := flag.String("regen", "", "regenerate maven-metadata.xml for the given version directory (path relative to -root) and exit immediately, without starting the server -- use after placing artifact files directly on disk")
	maxUploadFlag := flag.Int64("max-upload-size", 500*1024*1024, "maximum PUT request body size in bytes, enforced by the server itself regardless of any reverse-proxy limit (0 = unlimited)")
	allowProxies := flag.Bool("allow-proxies", false, "expose GET /-/hub/events, a long-lived stream of change notifications for Hub Proxy instances (see -upstream)")
	upstream := flag.String("upstream", "", "run as a Hub Proxy: upstream Hub base URL to cache from and receive change notifications from. Switches into proxy mode -- PUT/DELETE always rejected")
	upstreamUser := flag.String("upstream-user", "", "username for authenticating to -upstream (independent of -user/-pass, which gate this proxy's own clients)")
	upstreamPass := flag.String("upstream-pass", "", "password for authenticating to -upstream")
	upstreamPassFile := flag.String("upstream-pass-file", "", "path to a file containing the -upstream password; overrides -upstream-pass")
	syncInterval := flag.Duration("sync-interval", 60*time.Second, "poll-fallback interval used only while the push event stream to -upstream is disconnected")
	joinTimeout := flag.Duration("join-timeout", 30*time.Second, "how long to wait for the initial connection to -upstream before giving up and exiting")
	mirror := flag.Bool("mirror", false, "requires -upstream: eagerly pull and keep a complete copy of the upstream, not just what's requested (a Hub Mirror, vs. a lazily-caching Hub Proxy)")
	mirrorConcurrency := flag.Int("mirror-concurrency", 8, "requires -mirror: how many files to fetch in parallel during a full sync pass")
	flag.Parse()
	maxUploadSize = *maxUploadFlag

	abs, err := filepath.Abs(*root)
	if err != nil {
		log.Fatalf("resolving root: %v", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		log.Fatalf("creating root: %v", err)
	}
	repoRoot = abs

	if *upstream != "" && *regen != "" {
		log.Fatalf("-upstream cannot be combined with -regen")
	}
	if *upstream != "" && *readOnly {
		log.Fatalf("-upstream cannot be combined with -read-only (proxy mode is always read-only)")
	}
	if *upstream != "" && *allowProxies {
		log.Fatalf("-upstream cannot be combined with -allow-proxies (proxy chaining is not supported)")
	}
	if *mirror && *upstream == "" {
		log.Fatalf("-mirror requires -upstream")
	}

	if *regen != "" {
		target, ok := safePath(repoRoot, "/"+*regen)
		if !ok {
			log.Fatalf("-regen path escapes -root")
		}
		if info, err := os.Stat(target); err != nil || !info.IsDir() {
			log.Fatalf("-regen path must be an existing directory under -root: %v", err)
		}
		regenerateForVersionDir(target)
		return
	}

	if *upstream != "" {
		upstreamURL, err := url.Parse(*upstream)
		if err != nil || (upstreamURL.Scheme != "http" && upstreamURL.Scheme != "https") || upstreamURL.Host == "" {
			log.Fatalf("-upstream must be an http(s) URL: %v", err)
		}
		resolvedUpstreamPass, err := resolvePassword(*upstreamPass, *upstreamPassFile)
		if err != nil {
			log.Fatalf("reading upstream-pass-file: %v", err)
		}
		resolvedPass, err := resolvePassword(*pass, *passFile)
		if err != nil {
			log.Fatalf("reading pass-file: %v", err)
		}

		px := newProxyState(repoRoot, upstreamURL, *upstreamUser, resolvedUpstreamPass, *syncInterval, *mirror, *mirrorConcurrency)
		log.Printf("joining upstream %s (timeout %s)...", upstreamURL, *joinTimeout)
		bgCtx := context.Background()
		if err := px.join(bgCtx, *joinTimeout); err != nil {
			log.Fatalf("joining upstream: %v", err)
		}
		px.startBackgroundSync(bgCtx)
		if *mirror {
			log.Printf("hub mirror replicating %s from %s, serving on %s", repoRoot, upstreamURL, *addr)
		} else {
			log.Printf("hub proxy caching %s from %s, serving on %s", repoRoot, upstreamURL, *addr)
		}
		srv := &http.Server{
			Addr:              *addr,
			Handler:           newProxyHandler(px, *user, resolvedPass, *readAuth),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		log.Fatal(srv.ListenAndServe())
	}

	resolvedPass, err := resolvePassword(*pass, *passFile)
	if err != nil {
		log.Fatalf("reading pass-file: %v", err)
	}

	if *allowProxies {
		eventsBroadcaster = newEventBroadcaster()
	}

	log.Printf("maven repo serving %s on %s (snapshot-aware metadata enabled)", repoRoot, *addr)
	srv := &http.Server{
		Addr:    *addr,
		Handler: newHandler(*user, resolvedPass, *readAuth, *readOnly, *allowProxies),
		// ReadHeaderTimeout guards against slowloris-style connections that
		// trickle headers in to hold a connection open indefinitely.
		// ReadTimeout/WriteTimeout are deliberately left unset: they'd bound
		// the *entire* request/response duration, which would cap large
		// artifact uploads/downloads on a slow connection well below
		// -max-upload-size's actual byte limit.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

// newHandler builds the HTTP handler. repoRoot must be set before calling.
func newHandler(user, pass string, readAuth, readOnly, allowProxies bool) http.Handler {
	fileServer := http.FileServer(http.Dir(repoRoot))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowProxies && r.Method == http.MethodGet && r.URL.Path == "/-/hub/events" {
			handleEvents(w, r, user, pass)
			return
		}
		if allowProxies && r.Method == http.MethodGet && r.URL.Path == "/-/hub/manifest" {
			handleManifest(w, r, user, pass)
			return
		}
		isWrite := r.Method == http.MethodPut || r.Method == http.MethodDelete
		if readOnly && isWrite {
			http.Error(w, "repository is read-only", http.StatusMethodNotAllowed)
			return
		}
		if (isWrite || readAuth) && !authOK(r, user, pass) {
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
	})
}

// safePath maps a request path to an absolute path guaranteed to stay under root.
func safePath(root, urlPath string) (string, bool) {
	clean := filepath.Clean("/" + strings.TrimPrefix(urlPath, "/"))
	full := filepath.Join(root, clean)
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

// writeError logs the real error server-side (with the request that caused
// it) and returns only a generic message to the client, so internal details
// like filesystem paths never leak into a response body.
func writeError(w http.ResponseWriter, r *http.Request, status int, msg string, err error) {
	log.Printf("%s %s: %s: %v", r.Method, r.URL.Path, msg, err)
	http.Error(w, msg, status)
}

func handlePut(w http.ResponseWriter, r *http.Request) {
	full, ok := safePath(repoRoot, r.URL.Path)
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
		writeError(w, r, http.StatusInternalServerError, "could not create directory", err)
		return
	}
	f, err := os.Create(full)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "could not create file", err)
		return
	}

	body := io.Reader(r.Body)
	if maxUploadSize > 0 {
		body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		os.Remove(full) // don't leave a partial/truncated file behind
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "upload exceeds maximum allowed size", http.StatusRequestEntityTooLarge)
			return
		}
		writeError(w, r, http.StatusInternalServerError, "upload failed", err)
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
	full, ok := safePath(repoRoot, r.URL.Path)
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if err := os.Remove(full); err != nil {
		writeError(w, r, http.StatusNotFound, "not found", err)
		return
	}
	broadcastChanged(filepath.Dir(full))
	w.WriteHeader(http.StatusNoContent)
}

// resolvePassword returns passFile's trimmed contents when set, else pass as-is.
func resolvePassword(pass, passFile string) (string, error) {
	if passFile == "" {
		return pass, nil
	}
	data, err := os.ReadFile(passFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
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

// ---- proxy change notifications --------------------------------------------

const (
	eventBufferSize   = 8
	keepAliveInterval = 20 * time.Second
)

// hubEvent is pushed over GET /-/hub/events as one NDJSON line. Dir is a
// POSIX path relative to repoRoot; empty on a "ping" keep-alive.
type hubEvent struct {
	Type string `json:"type"` // "changed" | "ping"
	Dir  string `json:"dir,omitempty"`
}

// eventBroadcaster fans out hubEvents to every connected Hub Proxy. Its
// mutex is independent of the package-level mu (which only serializes
// filesystem writes) -- broadcasting never touches mu, so there's no
// lock-ordering hazard calling broadcast while mu is already held.
type eventBroadcaster struct {
	mu   sync.Mutex
	next uint64
	subs map[uint64]chan hubEvent
}

func newEventBroadcaster() *eventBroadcaster {
	return &eventBroadcaster{subs: make(map[uint64]chan hubEvent)}
}

func (b *eventBroadcaster) subscribe() (uint64, chan hubEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan hubEvent, eventBufferSize)
	b.subs[id] = ch
	return id, ch
}

func (b *eventBroadcaster) unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// broadcast delivers ev to every subscriber without blocking. A subscriber
// too slow to keep its buffer drained is dropped (and its channel closed)
// rather than allowed to stall this call -- a stuck Hub Proxy connection
// must never delay a write on the hub itself. Dropped proxies catch up via
// their own poll fallback.
func (b *eventBroadcaster) broadcast(ev hubEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			delete(b.subs, id)
			close(ch)
		}
	}
}

// broadcastChanged notifies connected Hub Proxies that dir (an absolute
// path under repoRoot) changed. A no-op when -allow-proxies isn't set.
func broadcastChanged(dir string) {
	if eventsBroadcaster == nil {
		return
	}
	rel, err := filepath.Rel(repoRoot, dir)
	if err != nil {
		return
	}
	eventsBroadcaster.broadcast(hubEvent{Type: "changed", Dir: filepath.ToSlash(rel)})
}

// handleEvents serves GET /-/hub/events: a long-lived NDJSON stream of
// hubEvents. Always requires user/pass when user is set, regardless of
// -read-auth -- joining is a privileged, resource-consuming action, not an
// anonymous read.
func handleEvents(w http.ResponseWriter, r *http.Request, user, pass string) {
	if !authOK(r, user, pass) {
		w.Header().Set("WWW-Authenticate", `Basic realm="maven"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	id, ch := eventsBroadcaster.subscribe()
	defer eventsBroadcaster.unsubscribe(id)

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()

	enc := json.NewEncoder(w)
	for {
		select {
		case ev, ok := <-ch:
			if !ok { // dropped as a slow subscriber
				return
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if err := enc.Encode(hubEvent{Type: "ping"}); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// manifestEntry is one line of GET /-/hub/manifest's NDJSON response.
type manifestEntry struct {
	Path string `json:"path"` // POSIX-relative to repoRoot
}

// handleManifest serves GET /-/hub/manifest: an NDJSON listing of every
// regular file under repoRoot (metadata and checksum sidecars included --
// a Hub Mirror wants a byte-identical copy of the whole tree, not a
// curated "real artifacts only" view). Same auth rule as handleEvents:
// always required when user is set, regardless of -read-auth. Unlike
// /-/hub/events this is a normal bounded request/response, not a
// long-lived stream.
func handleManifest(w http.ResponseWriter, r *http.Request, user, pass string) {
	if !authOK(r, user, pass) {
		w.Header().Set("WWW-Authenticate", `Basic realm="maven"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return nil
		}
		enc.Encode(manifestEntry{Path: filepath.ToSlash(rel)})
		return nil
	})
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
// is always the version dir, so hand off to regenerateForVersionDir.
func regenerateForArtifact(artifactFile string) {
	regenerateForVersionDir(filepath.Dir(artifactFile))
}

// regenerateForVersionDir refreshes the version-level snapshot metadata (when
// verDir is a -SNAPSHOT) and always the artifact-level versions metadata one
// level up. Also used by -regen for artifacts placed directly on disk instead
// of via PUT, where nothing has triggered regeneration yet.
func regenerateForVersionDir(verDir string) {
	if strings.HasSuffix(filepath.Base(verDir), "-SNAPSHOT") {
		regenSnapshot(verDir)
	}
	regenArtifact(filepath.Dir(verDir))
	broadcastChanged(verDir)
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
		broadcastChanged(dir)
		return
	}
	regenArtifact(dir)
	broadcastChanged(dir)
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
