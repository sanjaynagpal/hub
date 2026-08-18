package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// requireSh skips a test on a system with no sh on PATH -- notify()/reload
// both shell out via "sh -c", so tests exercising them need it available.
func requireSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not found on PATH")
	}
}

// writeTestCert writes a self-signed certificate with the given expiry to
// certDir/fullchain.pem, standing in for an installed ACME certificate in
// tests that only need readCertInfo/run's expiry logic, not a real CA
// round-trip. Returns the certificate's serial number.
func writeTestCert(t *testing.T, certDir string, notAfter time.Time) *big.Int {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(filepath.Join(certDir, "fullchain.pem"), pemBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	return serial
}

// notifyLoggerCmd is a -notify-cmd that appends one "event|days|domain" line
// per invocation to logFile, so tests can assert which events fired and how
// many times without needing a real notification channel.
func notifyLoggerCmd(logFile string) string {
	return fmt.Sprintf(`printf '%%s|%%s|%%s\n' "$NOTIFY_EVENT" "$NOTIFY_DAYS_REMAINING" "$NOTIFY_DOMAIN" >> "%s"`, logFile)
}

// checkOwnerOnlyPerm asserts 0600 permissions, but only on POSIX -- Windows'
// NTFS ACLs don't map onto the mode bits os.WriteFile/os.Chmod requested,
// and this tool's actual deployment target is always Linux (cross-compiled).
func checkOwnerOnlyPerm(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected %s to be 0600, got %v", path, info.Mode().Perm())
	}
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func bytesToBigInt(b []byte) *big.Int { return new(big.Int).SetBytes(b) }

func jwkToPublicKey(t testing.TB, j jwk) *ecdsa.PublicKey {
	t.Helper()
	x, err := base64.RawURLEncoding.DecodeString(j.X)
	if err != nil {
		t.Fatal(err)
	}
	y, err := base64.RawURLEncoding.DecodeString(j.Y)
	if err != nil {
		t.Fatal(err)
	}
	raw := append([]byte{0x04}, append(x, y...)...)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestThumbprintDeterministicAndDistinct(t *testing.T) {
	k1, k2 := mustKey(t), mustKey(t)
	t1a := thumbprint(&k1.PublicKey)
	t1b := thumbprint(&k1.PublicKey)
	if t1a != t1b {
		t.Fatalf("thumbprint not deterministic: %s vs %s", t1a, t1b)
	}
	if t1a == thumbprint(&k2.PublicKey) {
		t.Fatal("distinct keys produced the same thumbprint")
	}
	if t1a == "" {
		t.Fatal("thumbprint is empty")
	}
}

// TestJWSSignatureVerifiable checks the ES256 JWS this client produces uses
// the JOSE fixed-width r||s encoding (not ASN.1 DER) that any standard
// verifier -- including a real ACME server -- expects.
func TestJWSSignatureVerifiable(t *testing.T) {
	key := mustKey(t)
	j := publicJWK(&key.PublicKey)
	header := jwsHeader{Alg: "ES256", Jwk: &j, Nonce: "test-nonce", URL: "https://ca.example/new-order"}
	payload := []byte(`{"identifiers":[{"type":"dns","value":"example.com"}]}`)

	body, err := signJWS(key, header, payload)
	if err != nil {
		t.Fatal(err)
	}
	var parsed jwsBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parsed.Signature)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 64 {
		t.Fatalf("expected a 64-byte fixed-width r||s signature, got %d bytes", len(sig))
	}
	hash := sha256.Sum256([]byte(parsed.Protected + "." + parsed.Payload))
	if !ecdsa.Verify(&key.PublicKey, hash[:], bytesToBigInt(sig[:32]), bytesToBigInt(sig[32:])) {
		t.Fatal("signature does not verify against the account public key")
	}

	protectedJSON, err := base64.RawURLEncoding.DecodeString(parsed.Protected)
	if err != nil {
		t.Fatal(err)
	}
	var gotHeader jwsHeader
	if err := json.Unmarshal(protectedJSON, &gotHeader); err != nil {
		t.Fatal(err)
	}
	if gotHeader.Nonce != "test-nonce" || gotHeader.URL != header.URL || gotHeader.Alg != "ES256" {
		t.Fatalf("protected header round-trip mismatch: %+v", gotHeader)
	}
	if gotHeader.Jwk == nil {
		t.Fatal("expected embedded jwk when no kid is set")
	}
}

// TestPostAsGetEmptyPayload confirms POST-as-GET (nil payload) encodes as the
// literal empty string per RFC 8555 §6.3, not base64("null") or similar.
func TestPostAsGetEmptyPayload(t *testing.T) {
	key := mustKey(t)
	body, err := signJWS(key, jwsHeader{Alg: "ES256", Nonce: "n", URL: "https://ca.example/order/1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var parsed jwsBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Payload != "" {
		t.Fatalf("expected empty payload for POST-as-GET, got %q", parsed.Payload)
	}
}

func TestLoadOrCreateAccountKeyPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "account.key")

	k1, err := loadOrCreateAccountKey(path)
	if err != nil {
		t.Fatal(err)
	}
	checkOwnerOnlyPerm(t, path)

	k2, err := loadOrCreateAccountKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !k1.PublicKey.Equal(&k2.PublicKey) {
		t.Fatal("second load generated a different key instead of reusing the persisted one")
	}
}

func TestInstallCertAtomicAndPermissions(t *testing.T) {
	dir := t.TempDir()
	certDir := filepath.Join(dir, "certs", "example.com")

	if err := installCert(certDir, []byte("cert-v1"), []byte("key-v1")); err != nil {
		t.Fatal(err)
	}
	if err := installCert(certDir, []byte("cert-v2"), []byte("key-v2")); err != nil {
		t.Fatal(err)
	}

	cert, err := os.ReadFile(filepath.Join(certDir, "fullchain.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cert) != "cert-v2" {
		t.Fatalf("expected the second install to win, got %q", cert)
	}
	checkOwnerOnlyPerm(t, filepath.Join(certDir, "privkey.pem"))
	if _, err := os.Stat(filepath.Join(certDir, "privkey.pem.new")); !os.IsNotExist(err) {
		t.Fatal("temp file privkey.pem.new should have been renamed away, not left behind")
	}
}

// ---- end-to-end against a mock ACME server ----------------------------------

// mockACME implements just enough of RFC 8555 (directory, newNonce,
// newAccount, newOrder, authorization/challenge, finalize, certificate
// download) to drive obtainCertificate through a full HTTP-01 issuance,
// verifying every signed request against the account key it's given. Any
// protocol violation fails t via Errorf (safe from these handler goroutines,
// unlike Fatal) and the offending request gets a 400 so the client doesn't
// hang waiting for a response that will never look right.
type mockACME struct {
	t       *testing.T
	srv     *httptest.Server
	domain  string
	token   string
	webroot string

	mu         sync.Mutex
	nonces     map[string]bool
	accountKey *ecdsa.PublicKey
	accountKID string
	authzValid bool
	orderReady bool
	certKey    *rsa.PrivateKey
	issuedPEM  []byte
}

func newMockACME(t *testing.T, domain, webroot string) *mockACME {
	certKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &mockACME{t: t, domain: domain, token: "test-token-123", webroot: webroot, nonces: map[string]bool{}, certKey: certKey}

	mux := http.NewServeMux()
	mux.HandleFunc("/directory", m.handleDirectory)
	mux.HandleFunc("/new-nonce", m.handleNewNonce)
	mux.HandleFunc("/new-account", m.handleNewAccount)
	mux.HandleFunc("/new-order", m.handleNewOrder)
	mux.HandleFunc("/authz/1", m.handleAuthz)
	mux.HandleFunc("/chal/1", m.handleChallenge)
	mux.HandleFunc("/order/1", m.handleOrder)
	mux.HandleFunc("/finalize/1", m.handleFinalize)
	mux.HandleFunc("/cert/1", m.handleCert)
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockACME) url(path string) string { return m.srv.URL + path }

func (m *mockACME) newNonceValue() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := fmt.Sprintf("nonce-%d", len(m.nonces)+1)
	m.nonces[n] = true
	return n
}

func (m *mockACME) handleDirectory(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(directory{
		NewNonce:   m.url("/new-nonce"),
		NewAccount: m.url("/new-account"),
		NewOrder:   m.url("/new-order"),
	})
}

func (m *mockACME) handleNewNonce(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Replay-Nonce", m.newNonceValue())
	w.WriteHeader(http.StatusNoContent)
}

// verifyJWS parses and checks a signed request, reporting (not failing, so
// it's goroutine-safe) any protocol violation via t.Errorf. ok is false if
// the caller should abort the handler with an error response.
func (m *mockACME) verifyJWS(r *http.Request, requireKID bool) (header jwsHeader, payload []byte, ok bool) {
	var body jwsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		m.t.Errorf("mock server: decoding JWS envelope: %v", err)
		return jwsHeader{}, nil, false
	}
	protectedJSON, err := base64.RawURLEncoding.DecodeString(body.Protected)
	if err != nil {
		m.t.Errorf("mock server: decoding protected header: %v", err)
		return jwsHeader{}, nil, false
	}
	if err := json.Unmarshal(protectedJSON, &header); err != nil {
		m.t.Errorf("mock server: unmarshalling protected header: %v", err)
		return jwsHeader{}, nil, false
	}

	m.mu.Lock()
	validNonce := m.nonces[header.Nonce]
	delete(m.nonces, header.Nonce)
	m.mu.Unlock()
	if !validNonce {
		m.t.Errorf("mock server: request carried an unknown/reused nonce %q", header.Nonce)
		return jwsHeader{}, nil, false
	}

	pub := m.accountKey
	if requireKID {
		m.mu.Lock()
		kid := m.accountKID
		m.mu.Unlock()
		if header.Kid != kid {
			m.t.Errorf("mock server: expected kid %q, got %q", kid, header.Kid)
			return jwsHeader{}, nil, false
		}
	} else if header.Jwk != nil {
		pub = jwkToPublicKey(m.t, *header.Jwk)
	}
	if pub == nil {
		m.t.Errorf("mock server: no key available to verify against")
		return jwsHeader{}, nil, false
	}

	sigRaw, err := base64.RawURLEncoding.DecodeString(body.Signature)
	if err != nil || len(sigRaw) != 64 {
		m.t.Errorf("mock server: malformed signature")
		return jwsHeader{}, nil, false
	}
	hash := sha256.Sum256([]byte(body.Protected + "." + body.Payload))
	if !ecdsa.Verify(pub, hash[:], bytesToBigInt(sigRaw[:32]), bytesToBigInt(sigRaw[32:])) {
		m.t.Errorf("mock server: JWS signature did not verify")
		return jwsHeader{}, nil, false
	}

	if body.Payload != "" {
		payload, err = base64.RawURLEncoding.DecodeString(body.Payload)
		if err != nil {
			m.t.Errorf("mock server: decoding payload: %v", err)
			return jwsHeader{}, nil, false
		}
	}
	return header, payload, true
}

func (m *mockACME) handleNewAccount(w http.ResponseWriter, r *http.Request) {
	header, _, ok := m.verifyJWS(r, false)
	if !ok {
		http.Error(w, "bad jws", http.StatusBadRequest)
		return
	}
	if header.Jwk == nil {
		m.t.Errorf("mock server: newAccount request carried no jwk")
		http.Error(w, "missing jwk", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.accountKey = jwkToPublicKey(m.t, *header.Jwk)
	m.accountKID = m.url("/acct/1")
	m.mu.Unlock()

	w.Header().Set("Location", m.accountKID)
	w.Header().Set("Replay-Nonce", m.newNonceValue())
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "valid"})
}

func (m *mockACME) handleNewOrder(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := m.verifyJWS(r, true); !ok {
		http.Error(w, "bad jws", http.StatusBadRequest)
		return
	}
	w.Header().Set("Location", m.url("/order/1"))
	w.Header().Set("Replay-Nonce", m.newNonceValue())
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(orderResource{
		Status:         "pending",
		Authorizations: []string{m.url("/authz/1")},
		Finalize:       m.url("/finalize/1"),
	})
}

func (m *mockACME) handleAuthz(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := m.verifyJWS(r, true); !ok {
		http.Error(w, "bad jws", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	status := "pending"
	if m.authzValid {
		status = "valid"
	}
	m.mu.Unlock()

	w.Header().Set("Replay-Nonce", m.newNonceValue())
	_ = json.NewEncoder(w).Encode(authzResource{
		Status: status,
		Identifier: struct {
			Value string `json:"value"`
		}{Value: m.domain},
		Challenges: []challengeResource{{Type: "http-01", URL: m.url("/chal/1"), Token: m.token, Status: status}},
	})
}

func (m *mockACME) handleChallenge(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := m.verifyJWS(r, true); !ok {
		http.Error(w, "bad jws", http.StatusBadRequest)
		return
	}

	// This stands in for the CA fetching http://<domain>/.well-known/acme-
	// challenge/<token> itself: confirm the client wrote the exact key
	// authorization RFC 8555 §8.3 specifies before considering it satisfied.
	wantKeyAuth := m.token + "." + thumbprint(m.accountKey)
	got, err := os.ReadFile(filepath.Join(m.webroot, ".well-known", "acme-challenge", m.token))
	if err != nil {
		m.t.Errorf("mock server: challenge file not found in webroot: %v", err)
	} else if string(got) != wantKeyAuth {
		m.t.Errorf("mock server: challenge file content = %q, want %q", got, wantKeyAuth)
	}

	m.mu.Lock()
	m.authzValid = true
	m.orderReady = true
	m.mu.Unlock()

	w.Header().Set("Replay-Nonce", m.newNonceValue())
	_ = json.NewEncoder(w).Encode(challengeResource{Type: "http-01", URL: m.url("/chal/1"), Token: m.token, Status: "valid"})
}

func (m *mockACME) handleOrder(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := m.verifyJWS(r, true); !ok {
		http.Error(w, "bad jws", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	status := "pending"
	if m.orderReady {
		status = "ready"
	}
	var certURL string
	if len(m.issuedPEM) > 0 {
		status = "valid"
		certURL = m.url("/cert/1")
	}
	m.mu.Unlock()

	w.Header().Set("Replay-Nonce", m.newNonceValue())
	_ = json.NewEncoder(w).Encode(orderResource{
		Status:         status,
		Authorizations: []string{m.url("/authz/1")},
		Finalize:       m.url("/finalize/1"),
		Certificate:    certURL,
	})
}

func (m *mockACME) handleFinalize(w http.ResponseWriter, r *http.Request) {
	_, payload, ok := m.verifyJWS(r, true)
	if !ok {
		http.Error(w, "bad jws", http.StatusBadRequest)
		return
	}
	var req struct {
		CSR string `json:"csr"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		m.t.Errorf("mock server: decoding finalize payload: %v", err)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	csrDER, err := base64.RawURLEncoding.DecodeString(req.CSR)
	if err != nil {
		m.t.Errorf("mock server: decoding CSR: %v", err)
		http.Error(w, "bad csr", http.StatusBadRequest)
		return
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		m.t.Errorf("mock server: parsing CSR: %v", err)
		http.Error(w, "bad csr", http.StatusBadRequest)
		return
	}
	if csr.Subject.CommonName != m.domain {
		m.t.Errorf("mock server: CSR CN = %q, want %q", csr.Subject.CommonName, m.domain)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: m.domain},
		DNSNames:     []string{m.domain},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, csr.PublicKey, m.certKey)
	if err != nil {
		m.t.Fatalf("mock server: creating certificate: %v", err) // setup failure, not a per-request assertion
	}

	m.mu.Lock()
	m.issuedPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	certURL := m.url("/cert/1")
	m.mu.Unlock()

	w.Header().Set("Replay-Nonce", m.newNonceValue())
	_ = json.NewEncoder(w).Encode(orderResource{
		Status:         "valid",
		Authorizations: []string{m.url("/authz/1")},
		Finalize:       m.url("/finalize/1"),
		Certificate:    certURL,
	})
}

func (m *mockACME) handleCert(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := m.verifyJWS(r, true); !ok {
		http.Error(w, "bad jws", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	pemBytes := m.issuedPEM
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/pem-certificate-chain")
	w.Header().Set("Replay-Nonce", m.newNonceValue())
	_, _ = w.Write(pemBytes)
}

func TestObtainCertificateEndToEnd(t *testing.T) {
	const domain = "maven.internal.example.com"
	webroot := t.TempDir()
	mock := newMockACME(t, domain, webroot)

	client := &acmeClient{http: mock.srv.Client(), accountKey: mustKey(t)}
	if err := client.bootstrap(mock.url("/directory")); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := client.ensureAccount("", nil); err != nil {
		t.Fatalf("ensureAccount: %v", err)
	}
	if client.kid == "" {
		t.Fatal("ensureAccount did not capture the account URL (kid)")
	}

	cfg := config{Domain: domain, Webroot: webroot, PollInterval: 5 * time.Millisecond, PollTimeout: 5 * time.Second}
	certPEM, keyPEM, err := obtainCertificate(client, cfg)
	if err != nil {
		t.Fatalf("obtainCertificate: %v", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "RSA PRIVATE KEY" {
		t.Fatalf("keyPEM did not decode to an RSA PRIVATE KEY block: %+v", keyBlock)
	}
	if _, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err != nil {
		t.Fatalf("parsing issued private key: %v", err)
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		t.Fatalf("certPEM did not decode to a CERTIFICATE block: %+v", certBlock)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("parsing issued certificate: %v", err)
	}
	if cert.Subject.CommonName != domain {
		t.Errorf("cert CN = %q, want %q", cert.Subject.CommonName, domain)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != domain {
		t.Errorf("cert DNSNames = %v, want [%s]", cert.DNSNames, domain)
	}

	if _, err := os.Stat(filepath.Join(webroot, ".well-known", "acme-challenge", mock.token)); !os.IsNotExist(err) {
		t.Error("challenge file should have been cleaned up after validation")
	}
}

// ---- expiry-aware renewal + notifications -----------------------------------

func TestReadCertInfoMissingIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	_, exists, err := readCertInfo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Fatal("expected exists=false when no fullchain.pem is present")
	}
}

func TestReadCertInfoParsesInstalledCert(t *testing.T) {
	dir := t.TempDir()
	notAfter := time.Now().Add(45 * 24 * time.Hour).Truncate(time.Second)
	serial := writeTestCert(t, dir, notAfter)

	info, exists, err := readCertInfo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected exists=true")
	}
	if !info.NotAfter.Equal(notAfter) {
		t.Errorf("NotAfter = %v, want %v", info.NotAfter, notAfter)
	}
	if info.Serial.Cmp(serial) != 0 {
		t.Errorf("Serial = %v, want %v", info.Serial, serial)
	}
}

func TestNotifyDedupBySerial(t *testing.T) {
	dir := t.TempDir()
	serial1 := big.NewInt(12345)
	serial2 := big.NewInt(67890)

	if alreadyNotified(dir, serial1) {
		t.Fatal("expected no marker before markNotified is called")
	}
	if err := markNotified(dir, serial1); err != nil {
		t.Fatal(err)
	}
	if !alreadyNotified(dir, serial1) {
		t.Fatal("expected the marker to match the serial it was written for")
	}
	if alreadyNotified(dir, serial2) {
		t.Fatal("a marker for one serial should not match a different serial (e.g. after renewal)")
	}
}

// TestRunFarFromExpiryIsNoOp uses an unreachable ACME directory URL as a
// tripwire: if run() incorrectly attempted a renewal despite the cert being
// nowhere near expiry, that request would fail loudly and the test would
// catch it via run()'s returned error.
func TestRunFarFromExpiryIsNoOp(t *testing.T) {
	certDir := t.TempDir()
	writeTestCert(t, certDir, time.Now().Add(90*24*time.Hour))
	logFile := filepath.Join(t.TempDir(), "notify.log")

	cfg := config{
		Domain:           "maven.internal.example.com",
		CertDir:          certDir,
		DirectoryURL:     "http://127.0.0.1:1/unreachable",
		NotifyCmd:        notifyLoggerCmd(logFile),
		RenewBeforeDays:  14,
		NotifyBeforeDays: 21,
	}
	if err := run(cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(logFile); !os.IsNotExist(err) {
		t.Error("expected no notification when the certificate is nowhere near expiry")
	}
}

func TestRunNotifiesUpcomingOnceThenDedups(t *testing.T) {
	requireSh(t)
	certDir := t.TempDir()
	writeTestCert(t, certDir, time.Now().Add(18*24*time.Hour)) // between renew(14) and notify(21) thresholds
	logFile := filepath.Join(t.TempDir(), "notify.log")

	cfg := config{
		Domain:           "maven.internal.example.com",
		CertDir:          certDir,
		DirectoryURL:     "http://127.0.0.1:1/unreachable", // tripwire, as above
		NotifyCmd:        notifyLoggerCmd(logFile),
		RenewBeforeDays:  14,
		NotifyBeforeDays: 21,
	}
	if err := run(cfg); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := run(cfg); err != nil {
		t.Fatalf("second run: %v", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("expected a renewal_upcoming notification to have been logged: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one notification across two runs (deduped by certificate serial), got %d: %v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "renewal_upcoming|") {
		t.Errorf("expected a renewal_upcoming event, got %q", lines[0])
	}
}

// TestRunRenewsAndNotifiesSuccess drives run() end-to-end against the mock
// ACME server from TestObtainCertificateEndToEnd, starting from a
// pre-installed certificate close enough to expiry to trigger renewal.
func TestRunRenewsAndNotifiesSuccess(t *testing.T) {
	requireSh(t)
	const domain = "maven.internal.example.com"
	webroot := t.TempDir()
	mock := newMockACME(t, domain, webroot)

	certDir := t.TempDir()
	writeTestCert(t, certDir, time.Now().Add(5*24*time.Hour)) // inside the 14-day renew window

	logFile := filepath.Join(t.TempDir(), "notify.log")
	cfg := config{
		Domain:           domain,
		DirectoryURL:     mock.url("/directory"),
		Webroot:          webroot,
		CertDir:          certDir,
		AccountKeyFile:   filepath.Join(t.TempDir(), "account.key"),
		NotifyCmd:        notifyLoggerCmd(logFile),
		RenewBeforeDays:  14,
		NotifyBeforeDays: 21,
		PollInterval:     5 * time.Millisecond,
		PollTimeout:      5 * time.Second,
	}
	if err := run(cfg); err != nil {
		t.Fatalf("run: %v", err)
	}

	info, exists, err := readCertInfo(certDir)
	if err != nil || !exists {
		t.Fatalf("expected a renewed certificate to be installed: exists=%v err=%v", exists, err)
	}
	if daysUntil(info.NotAfter) > 2 {
		t.Errorf("installed certificate still has %d days left -- expected the mock's freshly issued (~24h) cert, renewal doesn't seem to have happened", daysUntil(info.NotAfter))
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("expected a renewal_succeeded notification: %v", err)
	}
	if !strings.Contains(string(data), "renewal_succeeded|") {
		t.Errorf("expected a renewal_succeeded event in the notify log, got %q", data)
	}
}

func TestRunNotifiesFailureWhenRenewalFails(t *testing.T) {
	requireSh(t)
	certDir := t.TempDir()
	writeTestCert(t, certDir, time.Now().Add(5*24*time.Hour)) // inside the renew window, so renewal is attempted
	logFile := filepath.Join(t.TempDir(), "notify.log")

	cfg := config{
		Domain:           "maven.internal.example.com",
		CertDir:          certDir,
		DirectoryURL:     "http://127.0.0.1:1/unreachable",
		AccountKeyFile:   filepath.Join(t.TempDir(), "account.key"),
		NotifyCmd:        notifyLoggerCmd(logFile),
		RenewBeforeDays:  14,
		NotifyBeforeDays: 21,
	}
	if err := run(cfg); err == nil {
		t.Fatal("expected run to return an error when the ACME directory is unreachable")
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("expected a renewal_failed notification: %v", err)
	}
	if !strings.Contains(string(data), "renewal_failed|") {
		t.Errorf("expected a renewal_failed event in the notify log, got %q", data)
	}
}

// TestRunForceBypassesExpiryCheck seeds a certificate nowhere near expiry --
// the same setup TestRunFarFromExpiryIsNoOp uses to prove a no-op -- but with
// -force set, and drives it against a real mock ACME server to confirm the
// certificate actually gets renewed anyway, ignoring -renew-before-days/
// -notify-before-days entirely.
func TestRunForceBypassesExpiryCheck(t *testing.T) {
	requireSh(t)
	const domain = "maven.internal.example.com"
	webroot := t.TempDir()
	mock := newMockACME(t, domain, webroot)

	certDir := t.TempDir()
	writeTestCert(t, certDir, time.Now().Add(90*24*time.Hour)) // far from either threshold

	logFile := filepath.Join(t.TempDir(), "notify.log")
	cfg := config{
		Domain:           domain,
		DirectoryURL:     mock.url("/directory"),
		Webroot:          webroot,
		CertDir:          certDir,
		AccountKeyFile:   filepath.Join(t.TempDir(), "account.key"),
		NotifyCmd:        notifyLoggerCmd(logFile),
		RenewBeforeDays:  14,
		NotifyBeforeDays: 21,
		Force:            true,
		PollInterval:     5 * time.Millisecond,
		PollTimeout:      5 * time.Second,
	}
	if err := run(cfg); err != nil {
		t.Fatalf("run: %v", err)
	}

	info, exists, err := readCertInfo(certDir)
	if err != nil || !exists {
		t.Fatalf("expected a renewed certificate to be installed: exists=%v err=%v", exists, err)
	}
	if daysUntil(info.NotAfter) > 2 {
		t.Errorf("installed certificate still has %d days left -- expected -force to trigger renewal despite being far from either threshold", daysUntil(info.NotAfter))
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("expected a renewal_succeeded notification: %v", err)
	}
	if strings.Contains(string(data), "renewal_upcoming|") {
		t.Error("did not expect a renewal_upcoming notification when -force skips straight to renewal")
	}
	if !strings.Contains(string(data), "renewal_succeeded|") {
		t.Errorf("expected a renewal_succeeded event in the notify log, got %q", data)
	}
}
