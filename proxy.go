// Hub Proxy mode: a read-through cache in front of another Hub (-upstream).
//
// A Hub Proxy serves GET/HEAD from its own local cache when it has the
// artifact, and otherwise fetches it from -upstream and caches it before
// replying. It never accepts writes -- PUT/DELETE always 405; mvn deploy
// always targets the upstream Hub directly.
//
// Staleness is bounded two ways: the proxy holds a long-lived connection to
// the upstream's GET /-/hub/events and invalidates cached entries the
// instant a change is pushed, and falls back to periodically re-checking
// cached maven-metadata.xml files while that connection is down. The proxy
// must complete its initial join to the upstream before it starts serving
// its own traffic; once running, a later disconnect degrades to serving
// (possibly stale) cached content rather than failing requests.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	upstreamFetchTimeout = 60 * time.Second // bounds one-shot fetches/polls only, never the event stream
	reconnectBackoffMin  = 1 * time.Second
	reconnectBackoffMax  = 30 * time.Second
)

// proxyState holds a Hub Proxy's cache root, upstream connection info, and
// in-flight-fetch bookkeeping. A Hub Mirror (mirror == true) is a Hub Proxy
// that additionally pulls everything upfront and reconciles against the
// upstream's full file list rather than only what's been requested -- see
// runFullSync/runMirrorPollFallback.
type proxyState struct {
	root         string
	upstream     *url.URL
	upstreamUser string
	upstreamPass string
	httpClient   *http.Client // Timeout intentionally unset; every call bounds itself via context
	syncInterval time.Duration

	mirror            bool
	mirrorConcurrency int

	fetchMu  sync.Mutex
	inflight map[string]*inflightFetch // keyed by POSIX relative path

	connected atomic.Bool
}

type inflightFetch struct {
	done   chan struct{}
	result fetchResult
}

type fetchResult struct {
	status int // valid (200 or 404) only when err == nil
	err    error
}

func newProxyState(root string, upstream *url.URL, user, pass string, syncInterval time.Duration, mirror bool, mirrorConcurrency int) *proxyState {
	return &proxyState{
		root:              root,
		upstream:          upstream,
		upstreamUser:      user,
		upstreamPass:      pass,
		httpClient:        &http.Client{}, // no Timeout: would also bound the event stream
		syncInterval:      syncInterval,
		mirror:            mirror,
		mirrorConcurrency: mirrorConcurrency,
		inflight:          make(map[string]*inflightFetch),
	}
}

// newProxyHandler builds the HTTP handler for Hub Proxy mode: reads are
// served from cache (fetching-through on a miss), writes are always
// rejected.
func newProxyHandler(p *proxyState, user, pass string, readAuth bool) http.Handler {
	fileServer := http.FileServer(http.Dir(p.root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut || r.Method == http.MethodDelete {
			http.Error(w, "hub proxy is read-only", http.StatusMethodNotAllowed)
			return
		}
		if readAuth && !authOK(r, user, pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="maven"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			p.serveOrFetch(w, r, fileServer)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// serveOrFetch serves r from the local cache, fetching-and-caching from
// upstream first on a miss.
func (p *proxyState) serveOrFetch(w http.ResponseWriter, r *http.Request, fileServer http.Handler) {
	full, ok := safePath(p.root, r.URL.Path)
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	info, statErr := os.Stat(full)

	// Directory-shaped requests can't be cached at a file path -- proxy
	// straight through without ever touching the fetch-and-cache machinery.
	if strings.HasSuffix(r.URL.Path, "/") || (statErr == nil && info.IsDir()) {
		p.proxyThrough(w, r)
		return
	}

	if statErr == nil { // cache hit
		fileServer.ServeHTTP(w, r)
		return
	}

	relPath := strings.TrimPrefix(r.URL.Path, "/")
	result := p.fetchAndCache(r.Context(), relPath)
	switch {
	case result.err != nil:
		if errors.Is(result.err, context.DeadlineExceeded) {
			http.Error(w, "upstream timeout", http.StatusGatewayTimeout)
		} else {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		}
	case result.status == http.StatusNotFound:
		http.NotFound(w, r)
	default:
		fileServer.ServeHTTP(w, r) // now on disk; also correctly serves HEAD/Range
	}
}

// proxyThrough streams a GET straight from upstream without caching it --
// used only for directory-shaped requests, which can't share a path with a
// cached file.
func (p *proxyState) proxyThrough(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), upstreamFetchTimeout)
	defer cancel()
	body, status, err := p.doUpstreamGet(ctx, strings.TrimPrefix(r.URL.Path, "/"))
	if err != nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer body.Close()
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		io.Copy(w, body)
	}
}

// fetchAndCache fetches relPath from upstream and stores it under root,
// collapsing concurrent requests for the same path into a single upstream
// fetch (a hand-rolled singleflight -- no third-party dependency).
//
// The shared fetch runs on a context detached from any individual caller,
// so one caller disconnecting can't cancel the fetch for others still
// waiting on it; ctx here only bounds how long *this* caller is willing to
// wait for the (shared) result.
func (p *proxyState) fetchAndCache(ctx context.Context, relPath string) fetchResult {
	p.fetchMu.Lock()
	if f, ok := p.inflight[relPath]; ok {
		p.fetchMu.Unlock()
		select {
		case <-f.done:
			return f.result
		case <-ctx.Done():
			return fetchResult{err: ctx.Err()}
		}
	}
	f := &inflightFetch{done: make(chan struct{})}
	p.inflight[relPath] = f
	p.fetchMu.Unlock()

	fetchCtx, cancel := context.WithTimeout(context.Background(), upstreamFetchTimeout)
	defer cancel()
	f.result = p.doFetchAndStore(fetchCtx, relPath)

	p.fetchMu.Lock()
	delete(p.inflight, relPath) // unconditional: a failed fetch must be retryable
	p.fetchMu.Unlock()
	close(f.done)

	return f.result
}

// doFetchAndStore performs one unconditional GET against upstream and, on
// 200, atomically persists the body under root. It never forwards a
// triggering request's Range/If-* headers -- doing so could persist a
// partial body as if it were the whole file, permanently corrupting the
// cache entry.
func (p *proxyState) doFetchAndStore(ctx context.Context, relPath string) fetchResult {
	body, status, err := p.doUpstreamGet(ctx, relPath)
	if err != nil {
		return fetchResult{err: err}
	}
	defer body.Close()
	if status != http.StatusOK {
		io.Copy(io.Discard, body)
		return fetchResult{status: status}
	}

	full, ok := safePath(p.root, "/"+relPath)
	if !ok {
		return fetchResult{err: errors.New("bad cache path")}
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fetchResult{err: err}
	}
	// Temp file in the same directory as the destination: required for an
	// atomic rename (a different volume/tmpdir would make Rename non-atomic
	// or fail outright), and so concurrent readers never see a partial file.
	tmp, err := os.CreateTemp(dir, ".hub-fetch-*")
	if err != nil {
		return fetchResult{err: err}
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fetchResult{err: err}
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fetchResult{err: err}
	}
	if err := renameRetry(tmpName, full); err != nil {
		os.Remove(tmpName)
		return fetchResult{err: err}
	}
	return fetchResult{status: http.StatusOK}
}

// renameRetry renames oldpath to newpath, retrying briefly on a Windows
// sharing violation -- the same transient-lock hazard removeRetry guards
// against, here hitting the rename's destination instead of a delete.
func renameRetry(oldpath, newpath string) error {
	var err error
	for i := 0; i < removeRetries; i++ {
		if err = os.Rename(oldpath, newpath); err == nil {
			return nil
		}
		time.Sleep(removeRetryDelay)
	}
	return err
}

// doUpstreamGet issues one GET against upstream for relPath, authenticated
// with the proxy's upstream credentials.
func (p *proxyState) doUpstreamGet(ctx context.Context, relPath string) (io.ReadCloser, int, error) {
	u := *p.upstream
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + relPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	if p.upstreamUser != "" {
		req.SetBasicAuth(p.upstreamUser, p.upstreamPass)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.StatusCode, nil
}

// join performs the mandatory initial connection to the upstream's event
// stream. It must succeed within timeout or the proxy refuses to start
// serving. Structured as a plain function returning error (rather than
// calling log.Fatal itself) so it's independently testable.
func (p *proxyState) join(parent context.Context, timeout time.Duration) error {
	streamCtx, streamCancel := context.WithCancel(parent)

	type joinResult struct {
		resp *http.Response
		err  error
	}
	resultCh := make(chan joinResult, 1)
	go func() {
		resp, err := p.connectEventStream(streamCtx)
		resultCh <- joinResult{resp, err}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			streamCancel()
			return res.err
		}
		p.connected.Store(true)
		go p.runEventLoop(parent, res.resp, streamCancel)
		return nil
	case <-time.After(timeout):
		streamCancel() // abort the hung dial; its eventual send lands in the buffered channel unread
		return errors.New("timed out connecting to upstream event stream")
	}
}

// connectEventStream opens GET /-/hub/events against upstream. The
// returned response's body must be consumed (or closed) by the caller.
func (p *proxyState) connectEventStream(ctx context.Context) (*http.Response, error) {
	u := *p.upstream
	u.Path = strings.TrimSuffix(u.Path, "/") + "/-/hub/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if p.upstreamUser != "" {
		req.SetBasicAuth(p.upstreamUser, p.upstreamPass)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, errors.New("upstream event stream: " + resp.Status)
	}
	return resp, nil
}

// runEventLoop owns the event stream for the life of the process: it
// consumes the already-connected initial stream, then reconnects with
// exponential backoff whenever the connection drops, until ctx is done.
func (p *proxyState) runEventLoop(ctx context.Context, initial *http.Response, cancel context.CancelFunc) {
	resp := initial
	for {
		p.consumeEventStream(ctx, resp)
		cancel()
		p.connected.Store(false)

		backoff := reconnectBackoffMin
		for {
			if ctx.Err() != nil {
				return
			}
			var connCtx context.Context
			connCtx, cancel = context.WithCancel(ctx)
			r, err := p.connectEventStream(connCtx)
			if err == nil {
				resp = r
				p.connected.Store(true)
				break
			}
			cancel()
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff *= 2
			if backoff > reconnectBackoffMax {
				backoff = reconnectBackoffMax
			}
		}
	}
}

// consumeEventStream reads NDJSON lines off resp.Body until it ends,
// invalidating cache entries as "changed" events arrive. Returns when the
// stream closes or ctx is done.
func (p *proxyState) consumeEventStream(ctx context.Context, resp *http.Response) {
	defer resp.Body.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			resp.Body.Close() // unblock the Scan below on shutdown
		case <-done:
		}
	}()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		var ev hubEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type == "changed" {
			p.invalidate(ev.Dir)
		}
	}
}

// invalidate drops cached entries for dir (POSIX-relative to root) and the
// metadata one level up, mirroring regenerateForVersionDir's own "walk up"
// shape as deletion instead of regeneration -- so the next client request
// for anything under dir becomes a fresh miss.
func (p *proxyState) invalidate(dir string) {
	full, ok := safePath(p.root, "/"+dir)
	if !ok {
		return
	}
	removeRetry(full)

	parent := filepath.Dir(full)
	if parent == full || !strings.HasPrefix(parent, p.root) {
		return // clamp: don't let a malformed/empty dir walk outside root
	}
	removeMetadata(parent)
}

func removeMetadata(dir string) {
	removeRetry(filepath.Join(dir, "maven-metadata.xml"))
	removeRetry(filepath.Join(dir, "maven-metadata.xml.md5"))
	removeRetry(filepath.Join(dir, "maven-metadata.xml.sha1"))
}

const (
	removeRetries    = 10
	removeRetryDelay = 25 * time.Millisecond
)

// removeRetry removes path (file or directory tree), retrying briefly on a
// Windows sharing violation -- something else (most commonly real-time AV
// scanning) can transiently hold a handle open on a just-served cache file
// exactly when invalidation tries to remove it. A short retry clears it
// without giving up on the invalidation.
func removeRetry(path string) {
	for i := 0; i < removeRetries; i++ {
		if err := os.RemoveAll(path); err == nil {
			return
		}
		time.Sleep(removeRetryDelay)
	}
}

// runPollFallback periodically re-validates cached maven-metadata.xml files
// against upstream, but only while the push event stream is disconnected --
// it's purely a bound on staleness during an extended outage, not the
// primary sync mechanism.
func (p *proxyState) runPollFallback(ctx context.Context) {
	ticker := time.NewTicker(p.syncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !p.connected.Load() {
				p.pollOnce(ctx)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (p *proxyState) pollOnce(ctx context.Context) {
	filepath.WalkDir(p.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "maven-metadata.xml" {
			return nil
		}
		rel, err := filepath.Rel(p.root, path)
		if err != nil {
			return nil
		}
		relPath := filepath.ToSlash(rel)

		local, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		fetchCtx, cancel := context.WithTimeout(ctx, upstreamFetchTimeout)
		body, status, err := p.doUpstreamGet(fetchCtx, relPath)
		cancel()
		if err != nil {
			return nil
		}
		defer body.Close()
		if status != http.StatusOK {
			return nil
		}
		fresh, err := io.ReadAll(body)
		if err != nil {
			return nil
		}
		if !bytesEqual(local, fresh) {
			p.invalidate(filepath.ToSlash(filepath.Dir(rel)))
		}
		return nil
	})
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// startBackgroundSync launches the ongoing sync machinery. Called once,
// after a successful join, alongside the event loop already started by
// join. A Hub Mirror additionally does an eager full sync right away
// rather than waiting for the first poll-fallback tick (which only fires
// once disconnected), and reconciles against the upstream's full manifest
// instead of only re-validating already-cached metadata.
func (p *proxyState) startBackgroundSync(ctx context.Context) {
	if p.mirror {
		go p.runFullSync(ctx)
		go p.runMirrorPollFallback(ctx)
		return
	}
	go p.runPollFallback(ctx)
}

// fetchManifest retrieves the upstream's full file listing (manifestEntry
// is defined in main.go, shared with the hub-side handler that produces it).
func (p *proxyState) fetchManifest(ctx context.Context) ([]string, error) {
	u := *p.upstream
	u.Path = strings.TrimSuffix(u.Path, "/") + "/-/hub/manifest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if p.upstreamUser != "" {
		req.SetBasicAuth(p.upstreamUser, p.upstreamPass)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("upstream manifest: " + resp.Status)
	}

	var paths []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		var e manifestEntry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if e.Path != "" {
			paths = append(paths, e.Path)
		}
	}
	return paths, nil
}

// runFullSync performs one reconciliation pass immediately after join, so
// a Hub Mirror starts pulling everything right away.
func (p *proxyState) runFullSync(ctx context.Context) {
	p.reconcileWithManifest(ctx)
}

// runMirrorPollFallback is runPollFallback's Hub Mirror counterpart: same
// ticker/connected-flag gating, but reconciles against the full upstream
// manifest (catching newly-added AND upstream-deleted content) instead of
// only re-validating already-cached maven-metadata.xml files.
func (p *proxyState) runMirrorPollFallback(ctx context.Context) {
	ticker := time.NewTicker(p.syncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !p.connected.Load() {
				p.reconcileWithManifest(ctx)
			}
		case <-ctx.Done():
			return
		}
	}
}

// reconcileWithManifest brings root in line with what the upstream
// manifest says it actually has: fetches anything missing locally (bounded
// parallelism via mirrorConcurrency), and removes anything local that's no
// longer in the manifest (an upstream deletion that happened while
// disconnected -- the event stream already handles deletions in real
// time; this is the catch-up path for an extended outage). Already-present
// files are trusted and skipped, so repeated passes only transfer the
// delta: a changed-but-still-present file is handled by event-driven
// invalidation deleting the stale copy, which then falls into this same
// "missing locally" branch on the next pass.
func (p *proxyState) reconcileWithManifest(ctx context.Context) {
	manifest, err := p.fetchManifest(ctx)
	if err != nil {
		return // transient upstream hiccup; try again next pass
	}
	want := make(map[string]struct{}, len(manifest))
	for _, rel := range manifest {
		want[rel] = struct{}{}
	}

	sem := make(chan struct{}, p.mirrorConcurrency)
	var wg sync.WaitGroup
	for _, rel := range manifest {
		full, ok := safePath(p.root, "/"+rel)
		if !ok {
			continue
		}
		if _, err := os.Stat(full); err == nil {
			continue // already have it
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(rel string) {
			defer wg.Done()
			defer func() { <-sem }()
			p.fetchAndCache(ctx, rel)
		}(rel)
	}
	wg.Wait()

	filepath.WalkDir(p.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".hub-fetch-") {
			return nil
		}
		rel, err := filepath.Rel(p.root, path)
		if err != nil {
			return nil
		}
		relPath := filepath.ToSlash(rel)
		if _, ok := want[relPath]; !ok {
			removeRetry(path)
		}
		return nil
	})
}
