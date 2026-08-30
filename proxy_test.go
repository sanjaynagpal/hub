package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newUpstreamServer builds a standalone Hub (allow-proxies enabled) to act
// as a proxy's upstream, plus a request counter for the given path prefix.
func newUpstreamServer(t *testing.T, user, pass string) (*httptest.Server, *int32) {
	t.Helper()
	repoRoot = t.TempDir()
	eventsBroadcaster = newEventBroadcaster()
	t.Cleanup(func() { eventsBroadcaster = nil })

	var hits int32
	inner := newHandler(user, pass, false, false, true)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path != "/-/hub/events" {
			atomic.AddInt32(&hits, 1)
		}
		inner.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(handler)
	return srv, &hits
}

// newTestProxy builds a proxyState + handler pointed at upstream, joins it
// (failing the test if join doesn't succeed quickly), and returns both the
// running proxy server and a cancel func that must be called (via
// t.Cleanup, registered BEFORE the upstream's own Close) to stop its
// background event-stream connection before the upstream server closes.
// It does NOT call startBackgroundSync -- tests that need the poll
// fallback or a mirror's full sync call that themselves so they can
// control timing.
func newTestProxy(t *testing.T, upstream *httptest.Server, user, pass string) (*httptest.Server, *proxyState) {
	t.Helper()
	return newTestProxyState(t, upstream, user, pass, false)
}

// newTestMirror is newTestProxy's Hub Mirror counterpart.
func newTestMirror(t *testing.T, upstream *httptest.Server, user, pass string) (*httptest.Server, *proxyState) {
	t.Helper()
	return newTestProxyState(t, upstream, user, pass, true)
}

func newTestProxyState(t *testing.T, upstream *httptest.Server, user, pass string, mirror bool) (*httptest.Server, *proxyState) {
	t.Helper()
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	px := newProxyState(t.TempDir(), u, user, pass, 24*time.Hour, mirror, 8) // poll fallback not exercised by default
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := px.join(ctx, 5*time.Second); err != nil {
		t.Fatalf("join: %v", err)
	}
	if mirror {
		// A mirror is only meaningfully a mirror with its eager full sync
		// running -- unlike a plain proxy, which is fully functional via
		// on-demand fetch-through alone, so newTestProxy leaves background
		// sync to tests that specifically want to exercise it.
		px.startBackgroundSync(ctx)
	}
	srv := httptest.NewServer(newProxyHandler(px, "", "", false))
	t.Cleanup(srv.Close)
	return srv, px
}

func TestJoinFailsFastWhenUpstreamUnreachable(t *testing.T) {
	// Bind an ephemeral port, then close it -- connecting there fails
	// immediately (connection refused), well under the join timeout.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	u, _ := url.Parse("http://" + addr)
	px := newProxyState(t.TempDir(), u, "", "", time.Minute, false, 8)

	start := time.Now()
	err = px.join(context.Background(), 2*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("join against an unreachable upstream: got nil error, want one")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("join took %s to fail against an unreachable upstream, want well under its 2s timeout", elapsed)
	}
}

func TestCacheMissFetchesThroughAndCaches(t *testing.T) {
	upstream, hits := newUpstreamServer(t, "deployer", "s3cret")
	t.Cleanup(upstream.Close)
	mustPut(t, upstream.URL, snapDir+"/x.jar", "hello world")

	proxy, px := newTestProxy(t, upstream, "deployer", "s3cret")
	t.Cleanup(proxy.Close)

	code, body := get(t, proxy.URL, snapDir+"/x.jar")
	if code != http.StatusOK || body != "hello world" {
		t.Fatalf("GET through proxy: got %d %q, want 200 %q", code, body, "hello world")
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("upstream hits after first fetch: got %d, want 1", got)
	}

	full, ok := safePath(px.root, "/"+snapDir+"/x.jar")
	if !ok {
		t.Fatal("safePath rejected the cached artifact's own path")
	}
	if _, err := os.Stat(full); err != nil {
		t.Fatalf("fetched artifact was not persisted to the cache root: %v", err)
	}
}

func TestCacheHitAvoidsRefetch(t *testing.T) {
	upstream, hits := newUpstreamServer(t, "deployer", "s3cret")
	t.Cleanup(upstream.Close)
	mustPut(t, upstream.URL, snapDir+"/x.jar", "hello world")

	proxy, _ := newTestProxy(t, upstream, "deployer", "s3cret")
	t.Cleanup(proxy.Close)

	if code, _ := get(t, proxy.URL, snapDir+"/x.jar"); code != http.StatusOK {
		t.Fatalf("first GET: got %d", code)
	}
	if code, _ := get(t, proxy.URL, snapDir+"/x.jar"); code != http.StatusOK {
		t.Fatalf("second GET: got %d", code)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("upstream hits after two GETs of a cached file: got %d, want 1", got)
	}
}

func TestSingleflightCollapsesConcurrentMisses(t *testing.T) {
	upstream, hits := newUpstreamServer(t, "deployer", "s3cret")
	t.Cleanup(upstream.Close)
	mustPut(t, upstream.URL, snapDir+"/x.jar", "hello world")

	proxy, _ := newTestProxy(t, upstream, "deployer", "s3cret")
	t.Cleanup(proxy.Close)

	const n = 20
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = get(t, proxy.URL, snapDir+"/x.jar")
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("concurrent GET %d: got %d, want 200", i, c)
		}
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("upstream hits for %d concurrent misses of the same path: got %d, want 1", n, got)
	}
}

func TestProxyPutDeleteAlways405(t *testing.T) {
	upstream, _ := newUpstreamServer(t, "deployer", "s3cret")
	t.Cleanup(upstream.Close)

	proxy, _ := newTestProxy(t, upstream, "deployer", "s3cret")
	t.Cleanup(proxy.Close)

	if code := put(t, proxy.URL, snapDir+"/x.jar", "data", "deployer", "s3cret"); code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT on proxy: got %d, want 405", code)
	}
	req, err := http.NewRequest(http.MethodDelete, proxy.URL+"/"+snapDir+"/x.jar", nil)
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
		t.Fatalf("DELETE on proxy: got %d, want 405", resp.StatusCode)
	}
}

func TestEventDrivenInvalidationCausesRefetch(t *testing.T) {
	upstream, hits := newUpstreamServer(t, "deployer", "s3cret")
	t.Cleanup(upstream.Close)
	mustPut(t, upstream.URL, snapDir+"/x.jar", "version one")

	proxy, _ := newTestProxy(t, upstream, "deployer", "s3cret")
	t.Cleanup(proxy.Close)

	if code, body := get(t, proxy.URL, snapDir+"/x.jar"); code != http.StatusOK || body != "version one" {
		t.Fatalf("initial GET: got %d %q", code, body)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("upstream hits after initial GET: got %d, want 1", got)
	}

	mustPut(t, upstream.URL, snapDir+"/x.jar", "version two")

	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := get(t, proxy.URL, snapDir+"/x.jar")
		if code == http.StatusOK && body == "version two" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy never reflected the upstream change: last got %d %q", code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMirrorEagerlyPullsEverythingOnStartup(t *testing.T) {
	upstream, hits := newUpstreamServer(t, "deployer", "s3cret")
	t.Cleanup(upstream.Close)
	// Seed BEFORE the mirror exists, unlike the proxy tests -- a mirror
	// must discover and pull this without any client ever requesting it.
	mustPut(t, upstream.URL, snapDir+"/x.jar", "hello world")

	_, px := newTestMirror(t, upstream, "deployer", "s3cret")

	full, ok := safePath(px.root, "/"+snapDir+"/x.jar")
	if !ok {
		t.Fatal("safePath rejected the artifact's own path")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(full); err == nil {
			if string(data) != "hello world" {
				t.Fatalf("mirrored content: got %q, want %q", data, "hello world")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mirror never eagerly pulled the pre-existing upstream artifact")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(hits) < 1 {
		t.Fatal("mirror's eager sync never actually fetched from upstream")
	}
}

func TestMirrorPollFallbackDiscoversNewContent(t *testing.T) {
	upstream, _ := newUpstreamServer(t, "deployer", "s3cret")
	t.Cleanup(upstream.Close)

	_, px := newTestMirror(t, upstream, "deployer", "s3cret")

	// Added AFTER the mirror's initial sync already ran -- only a
	// manifest-based reconciliation pass (not the narrower proxy poll,
	// which only re-checks already-cached metadata) can discover this.
	mustPut(t, upstream.URL, "com/example/other/2.0.0/y.jar", "new content")

	px.reconcileWithManifest(context.Background())

	full, ok := safePath(px.root, "/com/example/other/2.0.0/y.jar")
	if !ok {
		t.Fatal("safePath rejected the new artifact's path")
	}
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("reconcileWithManifest did not pull the new upstream artifact: %v", err)
	}
	if string(data) != "new content" {
		t.Fatalf("reconciled content: got %q, want %q", data, "new content")
	}
}

func TestMirrorOrphanCleanup(t *testing.T) {
	upstream, _ := newUpstreamServer(t, "deployer", "s3cret")
	t.Cleanup(upstream.Close)
	mustPut(t, upstream.URL, snapDir+"/x.jar", "hello world")

	_, px := newTestMirror(t, upstream, "deployer", "s3cret")

	// A file that exists locally but was never part of the upstream
	// manifest -- e.g. an artifact removed upstream while this mirror was
	// disconnected. reconcileWithManifest must clean it up.
	orphan, ok := safePath(px.root, "/com/example/gone/9.9.9/orphan.jar")
	if !ok {
		t.Fatal("safePath rejected the orphan's path")
	}
	if err := os.MkdirAll(filepath.Dir(orphan), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	px.reconcileWithManifest(context.Background())

	if _, err := os.Stat(orphan); err == nil {
		t.Fatal("reconcileWithManifest left behind a file no longer present upstream")
	}
}

func TestProxyDirectoryRequestNotCached(t *testing.T) {
	upstream, _ := newUpstreamServer(t, "deployer", "s3cret")
	t.Cleanup(upstream.Close)
	mustPut(t, upstream.URL, snapDir+"/x.jar", "hello world")

	proxy, px := newTestProxy(t, upstream, "deployer", "s3cret")
	t.Cleanup(proxy.Close)

	code, _ := get(t, proxy.URL, snapDir+"/")
	if code != http.StatusOK {
		t.Fatalf("GET of a directory-shaped path through the proxy: got %d, want 200", code)
	}
	full, ok := safePath(px.root, "/"+snapDir)
	if !ok {
		t.Fatal("safePath rejected the version dir's own path")
	}
	if info, err := os.Stat(full); err == nil && !info.IsDir() {
		t.Fatalf("a directory-shaped request was cached as a file at %s", full)
	}
}
