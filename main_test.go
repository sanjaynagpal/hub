package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestServer sets repoRoot to a temp dir and returns a running test server
// with auth (deployer/s3cret) required for writes.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	repoRoot = t.TempDir()
	srv := httptest.NewServer(newHandler("deployer", "s3cret", false, false))
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
	srv := httptest.NewServer(newHandler("deployer", "s3cret", false, true))
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

func TestSafePathContainsTraversal(t *testing.T) {
	repoRoot = t.TempDir()
	for _, p := range []string{"/../../etc/passwd", "/a/../../b", "/../.."} {
		full, ok := safePath(p)
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
