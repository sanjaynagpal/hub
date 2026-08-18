// acmeclient — a minimal ACME (RFC 8555) client for provisioning the
// mavenrepo nginx front's TLS certificate from an internal ACME-capable CA.
// Go stdlib only, matching the rest of this repo.
//
// Replaces the earlier design (deploy/nginx/renew-cert.sh), which guessed at
// a bespoke REST contract for the internal CA that was never confirmed.
// ACME is a fixed protocol (RFC 8555) rather than a per-CA contract to
// reverse-engineer, so this client works against any conformant CA (step-ca,
// Vault PKI's ACME support, Let's Encrypt itself, etc.) without needing that
// confirmation. Uses the HTTP-01 challenge, since nginx already serves plain
// HTTP for the redirect-to-HTTPS vhost and can serve the challenge token from
// a static webroot alongside it.
//
// Every run checks the installed certificate's actual remaining validity
// before doing anything else (see docs/acme-protocol.md §9): far from
// expiry, it's a no-op; within -notify-before-days it fires -notify-cmd once
// as a heads-up; within -renew-before-days it renews and fires -notify-cmd
// again on success (or failure). This makes it safe to run frequently (e.g.
// daily) regardless of how long the CA actually makes its certificates
// valid for -- days, or months.
//
// Build: go build -o acmeclient ./cmd/acmeclient
//
//	Run:   ./acmeclient -domain maven.internal.example.com \
//	           -directory-url https://ca.internal.example.com/acme/directory
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	cfg := config{
		Domain:           envOr("DOMAIN", ""),
		DirectoryURL:     envOr("ACME_DIRECTORY_URL", ""),
		Webroot:          envOr("ACME_WEBROOT", "/var/www/acme-challenge"),
		CertDir:          envOr("CERT_DIR", ""),
		AccountKeyFile:   envOr("ACME_ACCOUNT_KEY_FILE", "/etc/nginx/certs/acme-account.key"),
		Contact:          envOr("ACME_CONTACT", ""),
		EABKeyID:         envOr("ACME_EAB_KID", ""),
		EABHMACKeyFile:   envOr("ACME_EAB_HMAC_KEY_FILE", ""),
		ReloadCmd:        envOr("RELOAD_CMD", "nginx -t && systemctl reload nginx"),
		NotifyCmd:        envOr("NOTIFY_CMD", ""),
		RenewBeforeDays:  envOrInt("ACME_RENEW_BEFORE_DAYS", 14),
		NotifyBeforeDays: envOrInt("ACME_NOTIFY_BEFORE_DAYS", 21),
		Force:            envOrBool("ACME_FORCE_RENEW", false),
	}
	flag.StringVar(&cfg.Domain, "domain", cfg.Domain, "domain to request a certificate for (env DOMAIN)")
	flag.StringVar(&cfg.DirectoryURL, "directory-url", cfg.DirectoryURL, "ACME directory URL of the internal CA (env ACME_DIRECTORY_URL)")
	flag.StringVar(&cfg.Webroot, "webroot", cfg.Webroot, "directory nginx serves for HTTP-01 challenges; .well-known/acme-challenge/<token> is written under it (env ACME_WEBROOT)")
	flag.StringVar(&cfg.CertDir, "cert-dir", cfg.CertDir, "output directory for privkey.pem/fullchain.pem (default /etc/nginx/certs/<domain>)")
	flag.StringVar(&cfg.AccountKeyFile, "account-key", cfg.AccountKeyFile, "path to the persistent ACME account key (created if missing) (env ACME_ACCOUNT_KEY_FILE)")
	flag.StringVar(&cfg.Contact, "contact", cfg.Contact, "optional contact email registered with the account (env ACME_CONTACT)")
	flag.StringVar(&cfg.EABKeyID, "eab-kid", cfg.EABKeyID, "external account binding key ID, if the CA requires EAB (env ACME_EAB_KID)")
	flag.StringVar(&cfg.EABHMACKeyFile, "eab-hmac-key-file", cfg.EABHMACKeyFile, "path to the base64url-encoded EAB HMAC key, if the CA requires EAB (env ACME_EAB_HMAC_KEY_FILE)")
	flag.StringVar(&cfg.ReloadCmd, "reload-cmd", cfg.ReloadCmd, "shell command run after a successful renewal (env RELOAD_CMD; empty to skip)")
	flag.StringVar(&cfg.NotifyCmd, "notify-cmd", cfg.NotifyCmd, "shell command run on renewal_upcoming/renewal_succeeded/renewal_failed events, with NOTIFY_* env vars set (env NOTIFY_CMD; empty to skip)")
	flag.IntVar(&cfg.RenewBeforeDays, "renew-before-days", cfg.RenewBeforeDays, "renew once the installed certificate has this many days or fewer left (env ACME_RENEW_BEFORE_DAYS)")
	flag.IntVar(&cfg.NotifyBeforeDays, "notify-before-days", cfg.NotifyBeforeDays, "fire -notify-cmd once (renewal_upcoming) when the certificate has this many days or fewer left, ahead of the actual renewal; must be >= -renew-before-days (env ACME_NOTIFY_BEFORE_DAYS)")
	flag.BoolVar(&cfg.Force, "force", cfg.Force, "renew unconditionally on every run, ignoring -renew-before-days/-notify-before-days and the installed certificate's actual expiry -- for operators who prefer the CA's rate limits (or none) over this tool's own scheduling (env ACME_FORCE_RENEW)")
	flag.DurationVar(&cfg.PollInterval, "poll-interval", 2*time.Second, "delay between ACME status polls")
	flag.DurationVar(&cfg.PollTimeout, "poll-timeout", 60*time.Second, "max time to wait for a challenge/order to become valid")
	flag.Parse()

	if cfg.Domain == "" {
		log.Fatal("-domain (or DOMAIN) is required")
	}
	if cfg.DirectoryURL == "" {
		log.Fatal("-directory-url (or ACME_DIRECTORY_URL) is required")
	}
	if cfg.CertDir == "" {
		cfg.CertDir = filepath.Join("/etc/nginx/certs", cfg.Domain)
	}
	if cfg.NotifyBeforeDays < cfg.RenewBeforeDays {
		log.Fatalf("-notify-before-days (%d) must be >= -renew-before-days (%d): the heads-up notice should fire before renewal does, not after", cfg.NotifyBeforeDays, cfg.RenewBeforeDays)
	}

	if err := run(cfg); err != nil {
		log.Fatal(err)
	}
}

type config struct {
	Domain           string
	DirectoryURL     string
	Webroot          string
	CertDir          string
	AccountKeyFile   string
	Contact          string
	EABKeyID         string
	EABHMACKeyFile   string
	ReloadCmd        string
	NotifyCmd        string
	RenewBeforeDays  int
	NotifyBeforeDays int
	Force            bool
	PollInterval     time.Duration
	PollTimeout      time.Duration
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envOrInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envOrBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// run checks the installed certificate's actual remaining validity and only
// acts if it's getting close to expiry -- see the package doc comment. This
// is what makes -renew-before-days/-notify-before-days meaningful regardless
// of whether the CA hands out day-scale or month-scale certificates: the
// client never assumes a lifetime, it just watches the real NotAfter.
//
// -force skips all of that and renews every run, matching this tool's
// original (pre-expiry-check) behavior -- for operators who'd rather rely on
// the CA's own rate limiting than have this tool decide when to renew, or
// who just want maximum key rotation frequency regardless of remaining
// validity.
func run(cfg config) error {
	if cfg.Force {
		log.Printf("-force set; renewing %s unconditionally", cfg.Domain)
	} else {
		info, exists, err := readCertInfo(cfg.CertDir)
		if err != nil {
			return fmt.Errorf("reading installed certificate: %w", err)
		}

		if exists {
			daysLeft := daysUntil(info.NotAfter)
			switch {
			case daysLeft > cfg.NotifyBeforeDays:
				log.Printf("certificate for %s has %d days left, nothing to do", cfg.Domain, daysLeft)
				return nil
			case daysLeft > cfg.RenewBeforeDays:
				if alreadyNotified(cfg.CertDir, info.Serial) {
					log.Printf("certificate for %s has %d days left, already sent the upcoming-renewal notice", cfg.Domain, daysLeft)
					return nil
				}
				msg := fmt.Sprintf("certificate for %s expires %s (%d days left) -- automatic renewal will run once %d days remain",
					cfg.Domain, info.NotAfter.Format(time.RFC3339), daysLeft, cfg.RenewBeforeDays)
				notify(cfg, "renewal_upcoming", msg, info.NotAfter, daysLeft)
				if err := markNotified(cfg.CertDir, info.Serial); err != nil {
					log.Printf("warning: could not persist notify dedup marker: %v", err)
				}
				return nil
			default:
				log.Printf("certificate for %s has %d days left (<= -renew-before-days=%d); renewing", cfg.Domain, daysLeft, cfg.RenewBeforeDays)
			}
		} else {
			log.Printf("no certificate installed yet for %s; obtaining one", cfg.Domain)
		}
	}

	if err := renew(cfg); err != nil {
		notify(cfg, "renewal_failed", fmt.Sprintf("renewing the certificate for %s failed: %v", cfg.Domain, err), time.Time{}, 0)
		return err
	}
	return nil
}

// renew does the actual ACME issuance and installs the result. Called either
// because no certificate exists yet, or because run found one within
// -renew-before-days of expiry.
func renew(cfg config) error {
	accountKey, err := loadOrCreateAccountKey(cfg.AccountKeyFile)
	if err != nil {
		return fmt.Errorf("account key: %w", err)
	}

	client := &acmeClient{http: &http.Client{Timeout: 30 * time.Second}, accountKey: accountKey}
	if err := client.bootstrap(cfg.DirectoryURL); err != nil {
		return fmt.Errorf("fetching ACME directory: %w", err)
	}

	eab, err := loadEAB(cfg)
	if err != nil {
		return fmt.Errorf("external account binding: %w", err)
	}
	if err := client.ensureAccount(cfg.Contact, eab); err != nil {
		return fmt.Errorf("ACME account: %w", err)
	}

	certPEM, keyPEM, err := obtainCertificate(client, cfg)
	if err != nil {
		return fmt.Errorf("obtaining certificate: %w", err)
	}

	if err := installCert(cfg.CertDir, certPEM, keyPEM); err != nil {
		return fmt.Errorf("installing certificate: %w", err)
	}
	log.Printf("issued certificate for %s, installed under %s", cfg.Domain, cfg.CertDir)

	if cfg.ReloadCmd != "" {
		out, err := exec.Command("sh", "-c", cfg.ReloadCmd).CombinedOutput()
		if err != nil {
			return fmt.Errorf("reload command %q failed: %w: %s", cfg.ReloadCmd, err, out)
		}
	}

	newInfo, err := parseCertInfo(certPEM)
	if err != nil {
		// The certificate is already installed and nginx reloaded; a parse
		// failure here only affects the success notification's wording, not
		// the outcome, so it's logged rather than turning a real success
		// into a reported failure.
		log.Printf("warning: could not parse issued certificate for the success notification: %v", err)
		notify(cfg, "renewal_succeeded", fmt.Sprintf("certificate for %s renewed", cfg.Domain), time.Time{}, 0)
		return nil
	}
	notify(cfg, "renewal_succeeded",
		fmt.Sprintf("certificate for %s renewed, now valid until %s", cfg.Domain, newInfo.NotAfter.Format(time.RFC3339)),
		newInfo.NotAfter, daysUntil(newInfo.NotAfter))
	return nil
}

// ---- certificate expiry tracking + renewal notifications --------------------

type certInfo struct {
	NotAfter time.Time
	Serial   *big.Int
}

// readCertInfo reads the currently-installed certificate, if any. exists is
// false (with a nil error) when no certificate has been installed yet --
// the normal state on a brand new host, not a failure.
func readCertInfo(certDir string) (certInfo, bool, error) {
	data, err := os.ReadFile(filepath.Join(certDir, "fullchain.pem"))
	if err != nil {
		if os.IsNotExist(err) {
			return certInfo{}, false, nil
		}
		return certInfo{}, false, err
	}
	info, err := parseCertInfo(data)
	if err != nil {
		return certInfo{}, false, err
	}
	return info, true, nil
}

// parseCertInfo extracts NotAfter/Serial from the leaf certificate, which
// RFC 8555 §7.4.2 requires the CA to send first in the chain.
func parseCertInfo(certPEM []byte) (certInfo, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return certInfo{}, errors.New("no PEM data found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return certInfo{}, err
	}
	return certInfo{NotAfter: cert.NotAfter, Serial: cert.SerialNumber}, nil
}

func daysUntil(t time.Time) int {
	return int(time.Until(t).Hours() / 24)
}

// The upcoming-renewal notice should fire once per certificate, not once per
// day it happens to still be within the notify window -- a marker file
// keyed on the current certificate's serial number gives that for free: a
// freshly renewed certificate has a different serial, so the next
// expiry cycle notifies again without any explicit reset needed.
func notifiedMarkerPath(certDir string) string {
	return filepath.Join(certDir, ".notified-serial")
}

func alreadyNotified(certDir string, serial *big.Int) bool {
	data, err := os.ReadFile(notifiedMarkerPath(certDir))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == serial.String()
}

func markNotified(certDir string, serial *big.Int) error {
	return os.WriteFile(notifiedMarkerPath(certDir), []byte(serial.String()), 0o644)
}

// notify runs -notify-cmd, if configured, with the event described via
// NOTIFY_* environment variables. Failures are logged, not returned: a
// broken notification hook shouldn't make a real certificate renewal (or a
// harmless no-op check) look like it failed.
func notify(cfg config, event, message string, expires time.Time, daysRemaining int) {
	if cfg.NotifyCmd == "" {
		return
	}
	cmd := exec.Command("sh", "-c", cfg.NotifyCmd)
	cmd.Env = append(os.Environ(),
		"NOTIFY_EVENT="+event,
		"NOTIFY_DOMAIN="+cfg.Domain,
		"NOTIFY_MESSAGE="+message,
		fmt.Sprintf("NOTIFY_DAYS_REMAINING=%d", daysRemaining),
	)
	if !expires.IsZero() {
		cmd.Env = append(cmd.Env, "NOTIFY_EXPIRES_AT="+expires.Format(time.RFC3339))
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("notify-cmd for event %q failed: %v: %s", event, err, out)
	}
}

// ---- account key + EAB ------------------------------------------------------

func loadOrCreateAccountKey(path string) (*ecdsa.PrivateKey, error) {
	if data, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("%s: not a PEM file", path)
		}
		return x509.ParseECPrivateKey(block.Bytes)
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

type eabConfig struct {
	KeyID   string
	HMACKey []byte
}

// loadEAB reads external-account-binding credentials if the caller configured
// them. Many private CAs (step-ca, Vault PKI's ACME support, etc.) require
// EAB to tie an ACME account to a pre-provisioned identity; public CAs
// generally don't. Returns (nil, nil) when unconfigured.
func loadEAB(cfg config) (*eabConfig, error) {
	if cfg.EABKeyID == "" && cfg.EABHMACKeyFile == "" {
		return nil, nil
	}
	if cfg.EABKeyID == "" || cfg.EABHMACKeyFile == "" {
		return nil, errors.New("-eab-kid and -eab-hmac-key-file must be set together")
	}
	raw, err := os.ReadFile(cfg.EABHMACKeyFile)
	if err != nil {
		return nil, err
	}
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("%s: expected base64url-encoded HMAC key: %w", cfg.EABHMACKeyFile, err)
	}
	return &eabConfig{KeyID: cfg.EABKeyID, HMACKey: key}, nil
}

// ---- JWS / JWK (RFC 7515 / RFC 7638, ES256 only) ----------------------------

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func publicJWK(pub *ecdsa.PublicKey) jwk {
	// Bytes() returns the uncompressed SEC1 point encoding: 0x04 || X || Y,
	// each coordinate fixed at 32 bytes for P-256.
	raw, err := pub.Bytes()
	if err != nil {
		panic(err) // only fails for an invalid key, which GenerateKey/ParseECPrivateKey never produce
	}
	return jwk{Kty: "EC", Crv: "P-256", X: b64(raw[1:33]), Y: b64(raw[33:65])}
}

// thumbprint implements RFC 7638: SHA-256 over the JWK's required members
// serialized with sorted keys and no whitespace.
func thumbprint(pub *ecdsa.PublicKey) string {
	j := publicJWK(pub)
	canon := fmt.Sprintf(`{"crv":"%s","kty":"%s","x":"%s","y":"%s"}`, j.Crv, j.Kty, j.X, j.Y)
	sum := sha256.Sum256([]byte(canon))
	return b64(sum[:])
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func fixedBytes(n *big.Int, size int) []byte {
	b := n.Bytes()
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}

type jwsHeader struct {
	Alg   string `json:"alg"`
	Jwk   *jwk   `json:"jwk,omitempty"`
	Kid   string `json:"kid,omitempty"`
	Nonce string `json:"nonce"`
	URL   string `json:"url"`
}

type jwsBody struct {
	Protected string `json:"protected"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// signJWS produces a flattened JWS per RFC 7515 §7.2.2, signed with key using
// ES256. payload == nil encodes POST-as-GET's required empty payload string.
func signJWS(key *ecdsa.PrivateKey, header jwsHeader, payload []byte) ([]byte, error) {
	protectedJSON, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	protected := b64(protectedJSON)
	payloadB64 := ""
	if payload != nil {
		payloadB64 = b64(payload)
	}
	signingInput := protected + "." + payloadB64
	hash := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, hash[:])
	if err != nil {
		return nil, err
	}
	sig := append(fixedBytes(r, 32), fixedBytes(s, 32)...)
	return json.Marshal(jwsBody{Protected: protected, Payload: payloadB64, Signature: b64(sig)})
}

// signHMACJWS is the HS256 variant used for the externalAccountBinding JWS,
// which is signed with the CA-issued EAB key rather than the account key.
func signHMACJWS(hmacKey []byte, header jwsHeader, payload []byte) ([]byte, error) {
	header.Alg = "HS256"
	protectedJSON, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	protected := b64(protectedJSON)
	payloadB64 := b64(payload)
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write([]byte(protected + "." + payloadB64))
	return json.Marshal(jwsBody{Protected: protected, Payload: payloadB64, Signature: b64(mac.Sum(nil))})
}

// ---- ACME protocol client ----------------------------------------------------

type directory struct {
	NewNonce   string `json:"newNonce"`
	NewAccount string `json:"newAccount"`
	NewOrder   string `json:"newOrder"`
	Meta       struct {
		ExternalAccountRequired bool `json:"externalAccountRequired"`
	} `json:"meta"`
}

type acmeProblem struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
}

func (p acmeProblem) Error() string { return fmt.Sprintf("%s: %s", p.Type, p.Detail) }

type acmeClient struct {
	http       *http.Client
	dir        directory
	accountKey *ecdsa.PrivateKey
	kid        string // account URL; set once ensureAccount succeeds
	nonce      string
}

func (c *acmeClient) bootstrap(directoryURL string) error {
	resp, err := c.http.Get(directoryURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: unexpected status %s", directoryURL, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&c.dir); err != nil {
		return err
	}
	return c.refreshNonce()
}

func (c *acmeClient) refreshNonce() error {
	resp, err := c.http.Head(c.dir.NewNonce)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	nonce := resp.Header.Get("Replay-Nonce")
	if nonce == "" {
		return errors.New("newNonce response carried no Replay-Nonce header")
	}
	c.nonce = nonce
	return nil
}

// postRaw sends a signed POST (or POST-as-GET when payload is nil) and
// returns the raw response body, retrying once on a badNonce error as the
// spec requires (the server hands back a fresh nonce alongside that error).
func (c *acmeClient) postRaw(url string, payload []byte, accept string) ([]byte, http.Header, error) {
	for attempt := range 2 {
		header := jwsHeader{Alg: "ES256", Nonce: c.nonce, URL: url}
		if c.kid != "" {
			header.Kid = c.kid
		} else {
			j := publicJWK(&c.accountKey.PublicKey)
			header.Jwk = &j
		}
		body, err := signJWS(c.accountKey, header, payload)
		if err != nil {
			return nil, nil, err
		}
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Content-Type", "application/jose+json")
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, nil, err
		}
		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, nil, err
		}
		if nonce := resp.Header.Get("Replay-Nonce"); nonce != "" {
			c.nonce = nonce
		}
		if resp.StatusCode >= 400 {
			var problem acmeProblem
			_ = json.Unmarshal(respBody, &problem)
			if problem.Type == "urn:ietf:params:acme:error:badNonce" && attempt == 0 {
				continue // fresh nonce was already captured above; retry once
			}
			if problem.Type != "" {
				return nil, nil, problem
			}
			return nil, nil, fmt.Errorf("POST %s: unexpected status %s: %s", url, resp.Status, respBody)
		}
		return respBody, resp.Header, nil
	}
	return nil, nil, errors.New("exhausted retries on repeated badNonce")
}

func (c *acmeClient) post(url string, payload []byte, out any) (http.Header, error) {
	body, headers, err := c.postRaw(url, payload, "")
	if err != nil {
		return nil, err
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return nil, err
		}
	}
	return headers, nil
}

func (c *acmeClient) postAsGet(url string, out any) error {
	_, err := c.post(url, nil, out)
	return err
}

func (c *acmeClient) ensureAccount(contact string, eab *eabConfig) error {
	payload := map[string]any{"termsOfServiceAgreed": true}
	if contact != "" {
		payload["contact"] = []string{"mailto:" + contact}
	}
	if eab != nil {
		eabJWS, err := signHMACJWS(eab.HMACKey, jwsHeader{Kid: eab.KeyID, URL: c.dir.NewAccount}, mustJSON(publicJWK(&c.accountKey.PublicKey)))
		if err != nil {
			return err
		}
		payload["externalAccountBinding"] = json.RawMessage(eabJWS)
	} else if c.dir.Meta.ExternalAccountRequired {
		return errors.New("CA requires external account binding; set -eab-kid and -eab-hmac-key-file")
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	headers, err := c.post(c.dir.NewAccount, data, nil)
	if err != nil {
		return err
	}
	c.kid = headers.Get("Location")
	if c.kid == "" {
		return errors.New("account request did not return a Location header")
	}
	return nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // v is always a fixed internal type; a marshal failure is a programmer error
	}
	return b
}

type orderResource struct {
	Status         string   `json:"status"`
	Authorizations []string `json:"authorizations"`
	Finalize       string   `json:"finalize"`
	Certificate    string   `json:"certificate"`
}

func (c *acmeClient) newOrder(domain string) (orderURL string, ord *orderResource, err error) {
	payload := mustJSON(map[string]any{
		"identifiers": []map[string]string{{"type": "dns", "value": domain}},
	})
	ord = &orderResource{}
	headers, err := c.post(c.dir.NewOrder, payload, ord)
	if err != nil {
		return "", nil, err
	}
	orderURL = headers.Get("Location")
	if orderURL == "" {
		return "", nil, errors.New("newOrder response did not return a Location header")
	}
	return orderURL, ord, nil
}

func (c *acmeClient) getOrder(url string) (*orderResource, error) {
	ord := &orderResource{}
	if err := c.postAsGet(url, ord); err != nil {
		return nil, err
	}
	return ord, nil
}

type challengeResource struct {
	Type   string `json:"type"`
	URL    string `json:"url"`
	Token  string `json:"token"`
	Status string `json:"status"`
}

type authzResource struct {
	Status     string `json:"status"`
	Identifier struct {
		Value string `json:"value"`
	} `json:"identifier"`
	Challenges []challengeResource `json:"challenges"`
}

func (c *acmeClient) getAuthorization(url string) (*authzResource, error) {
	az := &authzResource{}
	if err := c.postAsGet(url, az); err != nil {
		return nil, err
	}
	return az, nil
}

func http01Challenge(challenges []challengeResource) (challengeResource, bool) {
	for _, ch := range challenges {
		if ch.Type == "http-01" {
			return ch, true
		}
	}
	return challengeResource{}, false
}

// downloadCertificate fetches the issued certificate chain. RFC 8555 §7.4.2
// serves it as PEM, requested via POST-as-GET with an explicit Accept header.
func (c *acmeClient) downloadCertificate(url string) ([]byte, error) {
	body, _, err := c.postRaw(url, nil, "application/pem-certificate-chain")
	return body, err
}

// pollUntil re-fetches status via fetch every interval until it reports want,
// an error, or timeout elapses.
func pollUntil(fetch func() (status string, err error), want string, interval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		status, err := fetch()
		if err != nil {
			return err
		}
		if status == want {
			return nil
		}
		if status == "invalid" {
			return errors.New("ACME server marked this invalid")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for status %q, last seen %q", want, status)
		}
		time.Sleep(interval)
	}
}

// ---- certificate issuance ----------------------------------------------------

// obtainCertificate drives a full order through HTTP-01 validation and
// finalization, returning the issued certificate chain and its freshly
// generated private key, both PEM-encoded.
func obtainCertificate(client *acmeClient, cfg config) (certPEM, keyPEM []byte, err error) {
	orderURL, ord, err := client.newOrder(cfg.Domain)
	if err != nil {
		return nil, nil, fmt.Errorf("creating order: %w", err)
	}

	for _, azURL := range ord.Authorizations {
		az, err := client.getAuthorization(azURL)
		if err != nil {
			return nil, nil, fmt.Errorf("fetching authorization: %w", err)
		}
		if az.Status == "valid" {
			continue
		}
		if err := completeHTTP01(client, cfg, azURL, az); err != nil {
			return nil, nil, fmt.Errorf("authorization for %s: %w", az.Identifier.Value, err)
		}
	}

	if err := pollUntil(func() (string, error) {
		o, err := client.getOrder(orderURL)
		if err != nil {
			return "", err
		}
		*ord = *o
		return o.Status, nil
	}, "ready", cfg.PollInterval, cfg.PollTimeout); err != nil {
		// Some CAs skip straight from "pending" to "valid" once every
		// authorization is valid, never reporting "ready" -- treat that as
		// success too rather than failing the whole run.
		if ord.Status != "valid" {
			return nil, nil, fmt.Errorf("waiting for order to become ready: %w", err)
		}
	}

	certKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: cfg.Domain},
		DNSNames: []string{cfg.Domain},
	}, certKey)
	if err != nil {
		return nil, nil, err
	}

	if _, err := client.post(ord.Finalize, mustJSON(map[string]string{"csr": b64(csrDER)}), ord); err != nil {
		return nil, nil, fmt.Errorf("finalizing order: %w", err)
	}

	certURL := ord.Certificate
	if certURL == "" {
		if err := pollUntil(func() (string, error) {
			o, err := client.getOrder(orderURL)
			if err != nil {
				return "", err
			}
			*ord = *o
			return o.Status, nil
		}, "valid", cfg.PollInterval, cfg.PollTimeout); err != nil {
			return nil, nil, fmt.Errorf("waiting for order to be issued: %w", err)
		}
		certURL = ord.Certificate
	}
	if certURL == "" {
		return nil, nil, errors.New("order became valid but carried no certificate URL")
	}

	certPEM, err = client.downloadCertificate(certURL)
	if err != nil {
		return nil, nil, fmt.Errorf("downloading certificate: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(certKey)})
	return certPEM, keyPEM, nil
}

func completeHTTP01(client *acmeClient, cfg config, azURL string, az *authzResource) error {
	ch, ok := http01Challenge(az.Challenges)
	if !ok {
		return errors.New("CA offered no http-01 challenge")
	}

	keyAuth := ch.Token + "." + thumbprint(&client.accountKey.PublicKey)
	challengeDir := filepath.Join(cfg.Webroot, ".well-known", "acme-challenge")
	if err := os.MkdirAll(challengeDir, 0o755); err != nil {
		return err
	}
	challengeFile := filepath.Join(challengeDir, ch.Token)
	if err := os.WriteFile(challengeFile, []byte(keyAuth), 0o644); err != nil {
		return err
	}
	defer os.Remove(challengeFile)

	if _, err := client.post(ch.URL, []byte("{}"), nil); err != nil {
		return fmt.Errorf("notifying CA of challenge readiness: %w", err)
	}

	return pollUntil(func() (string, error) {
		a, err := client.getAuthorization(azURL)
		if err != nil {
			return "", err
		}
		return a.Status, nil
	}, "valid", cfg.PollInterval, cfg.PollTimeout)
}

// ---- installing the issued cert ---------------------------------------------

// installCert writes the key and chain to cert-dir, atomically swapping them
// into the filenames nginx's ssl_certificate/ssl_certificate_key expect, so a
// concurrently-reloading nginx never observes a half-written file.
func installCert(certDir string, certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(certDir, 0o750); err != nil {
		return err
	}
	keyTmp := filepath.Join(certDir, "privkey.pem.new")
	certTmp := filepath.Join(certDir, "fullchain.pem.new")
	if err := os.WriteFile(keyTmp, keyPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certTmp, certPEM, 0o644); err != nil {
		return err
	}
	if err := os.Rename(keyTmp, filepath.Join(certDir, "privkey.pem")); err != nil {
		return err
	}
	if err := os.Rename(certTmp, filepath.Join(certDir, "fullchain.pem")); err != nil {
		return err
	}
	return nil
}
