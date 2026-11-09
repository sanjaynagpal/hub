package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestServer sets repoRoot to a temp dir and returns a running test server
// with auth (deployer/s3cret) required for writes.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	repoRoot = t.TempDir()
	srv := httptest.NewServer(newHandler("deployer", "s3cret", false, false, false))
	t.Cleanup(srv.Close)
	return srv
}

func put(t *testing.T, base, path, body, user, pass string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, base+"/"+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func get(t *testing.T, base, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(base + "/" + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func mustPut(t *testing.T, base, path, body string) {
	t.Helper()
	if code := put(t, base, path, body, "deployer", "s3cret"); code != http.StatusCreated {
		t.Fatalf("PUT %s: got %d, want 201", path, code)
	}
}

func parseMeta(t *testing.T, xmlStr string) *metadata {
	t.Helper()
	var m metadata
	if err := xml.Unmarshal([]byte(xmlStr), &m); err != nil {
		t.Fatalf("unmarshal metadata: %v\n%s", err, xmlStr)
	}
	return &m
}

const snapDir = "com/example/mylib/1.0.0-SNAPSHOT"

func TestAuthRequiredForWrite(t *testing.T) {
	srv := newTestServer(t)
	if code := put(t, srv.URL, snapDir+"/x.jar", "data", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PUT: got %d, want 401", code)
	}
	if code := put(t, srv.URL, snapDir+"/x.jar", "data", "deployer", "s3cret"); code != http.StatusCreated {
		t.Fatalf("authenticated PUT: got %d, want 201", code)
	}
	// Reads stay anonymous.
	if code, _ := get(t, srv.URL, snapDir+"/x.jar"); code != http.StatusOK {
		t.Fatalf("anonymous GET: got %d, want 200", code)
	}
}

func TestResolvePasswordFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(path, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePassword("ignored", path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "s3cret" {
		t.Fatalf("got %q, want %q", got, "s3cret")
	}
}

func TestResolvePasswordFallsBackToFlag(t *testing.T) {
	got, err := resolvePassword("flagpass", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "flagpass" {
		t.Fatalf("got %q, want %q", got, "flagpass")
	}
}

func TestReadOnlyRejectsWrites(t *testing.T) {
	repoRoot = t.TempDir()
	srv := httptest.NewServer(newHandler("deployer", "s3cret", false, true, false))
	t.Cleanup(srv.Close)

	// Even with correct credentials, writes are rejected outright.
	if code := put(t, srv.URL, snapDir+"/x.jar", "data", "deployer", "s3cret"); code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT on read-only server: got %d, want 405", code)
	}
	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/"+snapDir+"/x.jar", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("deployer", "s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE on read-only server: got %d, want 405", resp.StatusCode)
	}

	// Reads still work.
	if code, _ := get(t, srv.URL, "/"); code != http.StatusOK {
		t.Fatalf("GET on read-only server: got %d, want 200", code)
	}
}

// TestRegenerateForVersionDir covers the -regen CLI path: artifact files
// placed directly on disk (bypassing PUT entirely, as a read-only repo
// populated out-of-band would) still get correct artifact-level metadata
// once regenerateForVersionDir is run against the version directory.
func TestRegenerateForVersionDir(t *testing.T) {
	repoRoot = t.TempDir()

	verDir := filepath.Join(repoRoot, "org", "erlang", "otp", "26.2.5.11")
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(verDir, "otp-26.2.5.11.jar"), []byte("jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(verDir, "otp-26.2.5.11.pom"), []byte("<project/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	regenerateForVersionDir(verDir)

	metaPath := filepath.Join(repoRoot, "org", "erlang", "otp", "maven-metadata.xml")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("artifact-level maven-metadata.xml not written: %v", err)
	}
	m := parseMeta(t, string(data))
	if m.Versioning.Latest != "26.2.5.11" {
		t.Fatalf("latest: got %q, want 26.2.5.11", m.Versioning.Latest)
	}
	if m.Versioning.Release != "26.2.5.11" {
		t.Fatalf("release: got %q, want 26.2.5.11", m.Versioning.Release)
	}
	if _, err := os.Stat(metaPath + ".sha1"); err != nil {
		t.Fatalf("missing metadata checksum sidecar: %v", err)
	}
}

func TestSnapshotMetadataRegeneration(t *testing.T) {
	srv := newTestServer(t)
	// Build 1
	mustPut(t, srv.URL, snapDir+"/mylib-1.0.0-20260813.101500-1.jar", "jar1")
	mustPut(t, srv.URL, snapDir+"/mylib-1.0.0-20260813.101500-1.pom", "<project/>")
	// Build 2 (newer) + a sources classifier
	mustPut(t, srv.URL, snapDir+"/mylib-1.0.0-20260813.102000-2.jar", "jar2")
	mustPut(t, srv.URL, snapDir+"/mylib-1.0.0-20260813.102000-2.pom", "<project/>")
	mustPut(t, srv.URL, snapDir+"/mylib-1.0.0-20260813.102000-2-sources.jar", "src2")

	code, body := get(t, srv.URL, snapDir+"/maven-metadata.xml")
	if code != http.StatusOK {
		t.Fatalf("GET metadata: got %d, want 200", code)
	}
	m := parseMeta(t, body)

	if m.GroupID != "com.example" || m.ArtifactID != "mylib" || m.Version != "1.0.0-SNAPSHOT" {
		t.Fatalf("coords wrong: %+v", m)
	}
	if m.Versioning.Snapshot == nil {
		t.Fatal("missing <snapshot> block")
	}
	if got := m.Versioning.Snapshot.BuildNumber; got != 2 {
		t.Fatalf("buildNumber: got %d, want 2 (must advance to newest build)", got)
	}
	if got := m.Versioning.Snapshot.Timestamp; got != "20260813.102000" {
		t.Fatalf("timestamp: got %q, want 20260813.102000", got)
	}
	if got := m.Versioning.LastUpdated; got != "20260813102000" {
		t.Fatalf("lastUpdated: got %q, want 20260813102000", got)
	}
	if m.Versioning.SnapshotVersions == nil {
		t.Fatal("missing <snapshotVersions>")
	}
	// Expect jar, sources:jar, pom — all pointing at the newest build value.
	want := map[string]string{
		"|jar":        "1.0.0-20260813.102000-2",
		"sources|jar": "1.0.0-20260813.102000-2",
		"|pom":        "1.0.0-20260813.102000-2",
	}
	got := map[string]string{}
	for _, sv := range m.Versioning.SnapshotVersions.SnapshotVersion {
		got[sv.Classifier+"|"+sv.Extension] = sv.Value
	}
	if len(got) != len(want) {
		t.Fatalf("snapshotVersions: got %v, want keys %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("snapshotVersion[%s]: got %q, want %q", k, got[k], v)
		}
	}
}

func TestClientMetadataIsIgnored(t *testing.T) {
	srv := newTestServer(t)
	mustPut(t, srv.URL, snapDir+"/mylib-1.0.0-20260813.101500-1.jar", "jar1")
	// Client tries to overwrite metadata with garbage; server must ignore it.
	mustPut(t, srv.URL, snapDir+"/maven-metadata.xml", "GARBAGE-NOT-XML")
	code, body := get(t, srv.URL, snapDir+"/maven-metadata.xml")
	if code != http.StatusOK {
		t.Fatalf("GET metadata: got %d", code)
	}
	if strings.Contains(body, "GARBAGE") {
		t.Fatalf("server served client's metadata instead of its own:\n%s", body)
	}
	if !strings.Contains(body, "<buildNumber>1</buildNumber>") {
		t.Fatalf("expected server-generated metadata with buildNumber 1:\n%s", body)
	}
}

func TestMetadataChecksumConsistency(t *testing.T) {
	srv := newTestServer(t)
	mustPut(t, srv.URL, snapDir+"/mylib-1.0.0-20260813.101500-1.jar", "jar1")

	_, xmlBody := get(t, srv.URL, snapDir+"/maven-metadata.xml")
	code, shaBody := get(t, srv.URL, snapDir+"/maven-metadata.xml.sha1")
	if code != http.StatusOK {
		t.Fatalf("GET .sha1: got %d, want 200", code)
	}
	sum := sha1.Sum([]byte(xmlBody))
	want := hex.EncodeToString(sum[:])
	if strings.TrimSpace(shaBody) != want {
		t.Fatalf("sha1 mismatch: served %q, computed %q", shaBody, want)
	}
}

func TestArtifactLevelVersions(t *testing.T) {
	srv := newTestServer(t)
	mustPut(t, srv.URL, snapDir+"/mylib-1.0.0-20260813.101500-1.jar", "jar")
	mustPut(t, srv.URL, "com/example/mylib/1.5.0/mylib-1.5.0.jar", "jar")
	mustPut(t, srv.URL, "com/example/mylib/2.0.0/mylib-2.0.0.jar", "jar")

	code, body := get(t, srv.URL, "com/example/mylib/maven-metadata.xml")
	if code != http.StatusOK {
		t.Fatalf("GET artifact metadata: got %d", code)
	}
	m := parseMeta(t, body)
	if m.Versioning.Versions == nil {
		t.Fatal("missing <versions>")
	}
	gotVers := strings.Join(m.Versioning.Versions.Version, ",")
	if gotVers != "1.0.0-SNAPSHOT,1.5.0,2.0.0" {
		t.Fatalf("versions: got %q, want 1.0.0-SNAPSHOT,1.5.0,2.0.0 (in order)", gotVers)
	}
	if m.Versioning.Latest != "2.0.0" {
		t.Fatalf("latest: got %q, want 2.0.0", m.Versioning.Latest)
	}
	if m.Versioning.Release != "2.0.0" {
		t.Fatalf("release: got %q, want 2.0.0", m.Versioning.Release)
	}
}

func TestLiteralSnapshot(t *testing.T) {
	srv := newTestServer(t)
	// Non-unique (literal) snapshot filenames.
	mustPut(t, srv.URL, snapDir+"/mylib-1.0.0-SNAPSHOT.jar", "jar")
	mustPut(t, srv.URL, snapDir+"/mylib-1.0.0-SNAPSHOT.pom", "<project/>")

	_, body := get(t, srv.URL, snapDir+"/maven-metadata.xml")
	m := parseMeta(t, body)
	if m.Versioning.SnapshotVersions == nil {
		t.Fatalf("missing snapshotVersions:\n%s", body)
	}
	for _, sv := range m.Versioning.SnapshotVersions.SnapshotVersion {
		if sv.Value != "1.0.0-SNAPSHOT" {
			t.Fatalf("literal snapshot value: got %q, want 1.0.0-SNAPSHOT", sv.Value)
		}
	}
	if m.Versioning.Snapshot == nil || !m.Versioning.Snapshot.LocalCopy {
		t.Fatalf("expected <snapshot><localCopy>true for literal snapshot:\n%s", body)
	}
}

func TestDelete(t *testing.T) {
	srv := newTestServer(t)
	mustPut(t, srv.URL, snapDir+"/mylib-1.0.0-20260813.101500-1.jar", "jar")
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/"+snapDir+"/mylib-1.0.0-20260813.101500-1.jar", nil)
	req.SetBasicAuth("deployer", "s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: got %d, want 204", resp.StatusCode)
	}
	if code, _ := get(t, srv.URL, snapDir+"/mylib-1.0.0-20260813.101500-1.jar"); code != http.StatusNotFound {
		t.Fatalf("after DELETE GET: got %d, want 404", code)
	}
}

func TestUploadSizeLimit(t *testing.T) {
	repoRoot = t.TempDir()
	orig := maxUploadSize
	maxUploadSize = 8 // bytes
	t.Cleanup(func() { maxUploadSize = orig })
	srv := httptest.NewServer(newHandler("deployer", "s3cret", false, false, false))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/"+snapDir+"/x.jar", strings.NewReader("this body is way over the limit"))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("deployer", "s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized PUT: got %d, want 413", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, snapDir, "x.jar")); !os.IsNotExist(err) {
		t.Fatalf("expected no partial file left behind, stat error: %v", err)
	}
}

func TestDeleteErrorDoesNotLeakServerPath(t *testing.T) {
	srv := newTestServer(t)
	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/"+snapDir+"/does-not-exist.jar", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("deployer", "s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE missing file: got %d, want 404", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), repoRoot) {
		t.Fatalf("error response leaked the server-side repo path: %s", b)
	}
}

func TestAccessLogMiddleware(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("hi"))
	})
	req := httptest.NewRequest(http.MethodGet, "/foo/bar", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()

	accessLogMiddleware(inner).ServeHTTP(rec, req)

	line := buf.String()
	for _, want := range []string{"127.0.0.1:12345", "GET", "/foo/bar", "418", "2b"} {
		if !strings.Contains(line, want) {
			t.Fatalf("access log line %q missing %q", line, want)
		}
	}
}

func TestAccessLogMiddlewarePreservesFlusher(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("ResponseWriter passed through accessLogMiddleware lost http.Flusher")
		}
	})
	req := httptest.NewRequest(http.MethodGet, "/-/hub/events", nil)
	accessLogMiddleware(inner).ServeHTTP(httptest.NewRecorder(), req)
}

func TestSafePathContainsTraversal(t *testing.T) {
	repoRoot = t.TempDir()
	for _, p := range []string{"/../../etc/passwd", "/a/../../b", "/../.."} {
		full, ok := safePath(repoRoot, p)
		if !ok {
			continue // rejected outright is also fine
		}
		if full != repoRoot && !strings.HasPrefix(full, repoRoot+string(os.PathSeparator)) {
			t.Fatalf("safePath(%q) escaped root: %q", p, full)
		}
	}
}

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "2.0.0", -1},
		{"1.5.0", "2.0.0", -1},
		{"1.9.0", "1.10.0", -1}, // numeric, not lexical
		{"1.0.0-SNAPSHOT", "1.0.0", -1},
		{"1.0.0", "1.0.0-SNAPSHOT", 1},
		{"2.0.0", "2.0.0", 0},
	}
	for _, c := range cases {
		if got := compareVersion(c.a, c.b); got != c.want {
			t.Errorf("compareVersion(%q,%q)=%d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// ---- proxy change notifications ---------------------------------------

// newProxyableTestServer is like newTestServer but with -allow-proxies
// enabled (an eventsBroadcaster wired in).
func newProxyableTestServer(t *testing.T, user, pass string) *httptest.Server {
	t.Helper()
	repoRoot = t.TempDir()
	eventsBroadcaster = newEventBroadcaster()
	t.Cleanup(func() { eventsBroadcaster = nil })
	srv := httptest.NewServer(newHandler(user, pass, false, false, true))
	t.Cleanup(srv.Close)
	return srv
}

// subscribeEvents opens a streaming GET to /-/hub/events and returns a
// channel of decoded events plus the response's status code.
func subscribeEvents(t *testing.T, base, user, pass string) (status int, events <-chan hubEvent, cancel func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/-/hub/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /-/hub/events: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return resp.StatusCode, nil, cancel
	}
	ch := make(chan hubEvent, 16)
	go func() {
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			var ev hubEvent
			if err := json.Unmarshal(scanner.Bytes(), &ev); err == nil {
				ch <- ev
			}
		}
	}()
	return resp.StatusCode, ch, cancel
}

// waitForChanged reads events until a "changed" event for dir arrives, a
// "changed" for a different dir shows up too many times, or timeout elapses.
func waitForChanged(t *testing.T, events <-chan hubEvent, dir string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-events:
			if ev.Type == "changed" && ev.Dir == dir {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for changed event on %q", dir)
		}
	}
}

func TestEventsGatedByAllowProxies(t *testing.T) {
	srv := newTestServer(t) // allowProxies=false
	code, _, cancel := subscribeEvents(t, srv.URL, "", "")
	defer cancel()
	if code != http.StatusNotFound {
		t.Fatalf("GET /-/hub/events with -allow-proxies off: got %d, want 404 (same as any other missing path)", code)
	}
}

func TestEventsRequiresAuthWhenUserSet(t *testing.T) {
	srv := newProxyableTestServer(t, "deployer", "s3cret")

	code, _, cancel := subscribeEvents(t, srv.URL, "", "")
	cancel()
	if code != http.StatusUnauthorized {
		t.Fatalf("GET /-/hub/events without credentials: got %d, want 401", code)
	}

	code, _, cancel = subscribeEvents(t, srv.URL, "deployer", "s3cret")
	defer cancel()
	if code != http.StatusOK {
		t.Fatalf("GET /-/hub/events with credentials: got %d, want 200", code)
	}
}

func TestEventsOpenWhenUserEmpty(t *testing.T) {
	srv := newProxyableTestServer(t, "", "")
	code, _, cancel := subscribeEvents(t, srv.URL, "", "")
	defer cancel()
	if code != http.StatusOK {
		t.Fatalf("GET /-/hub/events with no -user configured: got %d, want 200", code)
	}
}

func TestBroadcastOnArtifactPut(t *testing.T) {
	srv := newProxyableTestServer(t, "deployer", "s3cret")
	_, events, cancel := subscribeEvents(t, srv.URL, "deployer", "s3cret")
	defer cancel()

	mustPut(t, srv.URL, snapDir+"/x.jar", "data")
	waitForChanged(t, events, snapDir, 5*time.Second)
}

func TestBroadcastOnDirectMetadataPut(t *testing.T) {
	srv := newProxyableTestServer(t, "deployer", "s3cret")
	mustPut(t, srv.URL, snapDir+"/x.jar", "data") // establish the version dir first

	_, events, cancel := subscribeEvents(t, srv.URL, "deployer", "s3cret")
	defer cancel()

	mustPut(t, srv.URL, snapDir+"/maven-metadata.xml", "<metadata/>")
	waitForChanged(t, events, snapDir, 5*time.Second)
}

func TestBroadcastOnDelete(t *testing.T) {
	srv := newProxyableTestServer(t, "deployer", "s3cret")
	mustPut(t, srv.URL, snapDir+"/x.jar", "data")

	_, events, cancel := subscribeEvents(t, srv.URL, "deployer", "s3cret")
	defer cancel()

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/"+snapDir+"/x.jar", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("deployer", "s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: got %d, want 204", resp.StatusCode)
	}
	waitForChanged(t, events, snapDir, 5*time.Second)
}

// TestBroadcastDropsSlowSubscriber proves broadcast() never blocks a caller
// on a subscriber that isn't draining its buffer -- it must drop (and close)
// that subscriber instead.
func TestBroadcastDropsSlowSubscriber(t *testing.T) {
	b := newEventBroadcaster()
	_, ch := b.subscribe()

	for i := 0; i < eventBufferSize+1; i++ { // one more than the buffer holds
		done := make(chan struct{})
		go func() {
			b.broadcast(hubEvent{Type: "changed", Dir: "x"})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("broadcast #%d blocked on a full, undrained subscriber buffer", i)
		}
	}

	closed := false
	for i := 0; i < eventBufferSize+2; i++ {
		if _, ok := <-ch; !ok {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("dropped subscriber's channel was never closed")
	}
}

// ---- manifest (GET /-/hub/manifest) ------------------------------------

// fetchManifest issues GET /-/hub/manifest and returns the status code plus
// the decoded relative paths (nil if the response wasn't NDJSON/200).
func fetchManifest(t *testing.T, base, user, pass string) (int, []string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/-/hub/manifest", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /-/hub/manifest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var paths []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		var e manifestEntry
		if err := json.Unmarshal(scanner.Bytes(), &e); err == nil {
			paths = append(paths, e.Path)
		}
	}
	return resp.StatusCode, paths
}

func TestManifestGatedByAllowProxies(t *testing.T) {
	srv := newTestServer(t) // allowProxies=false
	code, _ := fetchManifest(t, srv.URL, "", "")
	if code != http.StatusNotFound {
		t.Fatalf("GET /-/hub/manifest with -allow-proxies off: got %d, want 404 (same as any other missing path)", code)
	}
}

func TestManifestRequiresAuthWhenUserSet(t *testing.T) {
	srv := newProxyableTestServer(t, "deployer", "s3cret")
	if code, _ := fetchManifest(t, srv.URL, "", ""); code != http.StatusUnauthorized {
		t.Fatalf("GET /-/hub/manifest without credentials: got %d, want 401", code)
	}
	if code, _ := fetchManifest(t, srv.URL, "deployer", "s3cret"); code != http.StatusOK {
		t.Fatalf("GET /-/hub/manifest with credentials: got %d, want 200", code)
	}
}

func TestManifestOpenWhenUserEmpty(t *testing.T) {
	srv := newProxyableTestServer(t, "", "")
	if code, _ := fetchManifest(t, srv.URL, "", ""); code != http.StatusOK {
		t.Fatalf("GET /-/hub/manifest with no -user configured: got %d, want 200", code)
	}
}

func TestManifestListsAllFiles(t *testing.T) {
	srv := newProxyableTestServer(t, "deployer", "s3cret")
	mustPut(t, srv.URL, snapDir+"/x.jar", "data")

	// x.jar doesn't match the snapshot filename convention (no
	// -SNAPSHOT/timestamped suffix on the base name), so only the
	// artifact-level metadata one level up gets (re)generated -- not
	// version-level snapshot metadata. See regenSnapshot in main.go.
	_, paths := fetchManifest(t, srv.URL, "deployer", "s3cret")
	want := map[string]bool{
		snapDir + "/x.jar":                        false,
		"com/example/mylib/maven-metadata.xml":     false,
		"com/example/mylib/maven-metadata.xml.md5": false,
	}
	for _, p := range paths {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for p, found := range want {
		if !found {
			t.Errorf("manifest missing expected path %q (got %v)", p, paths)
		}
	}
}
