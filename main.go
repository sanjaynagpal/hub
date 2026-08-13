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

	log.Printf("maven repo serving %s on %s (snapshot-aware metadata enabled)", repoRoot, *addr)
	log.Fatal(http.ListenAndServe(*addr, newHandler(*user, *pass, *readAuth)))
}

// newHandler builds the HTTP handler. repoRoot must be set before calling.
func newHandler(user, pass string, readAuth bool) http.Handler {
	fileServer := http.FileServer(http.Dir(repoRoot))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isWrite := r.Method == http.MethodPut || r.Method == http.MethodDelete
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
