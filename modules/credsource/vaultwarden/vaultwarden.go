// Package vaultwarden is the Vaultwarden/Bitwarden credential connector — the `vw:` SecretRef scheme
// (spec/024 T-024-6, REQ-2404). It is the HOMELAB-tier backend beside the machine-plane OpenBao one:
// a real backend (no plaintext at rest, secrets fetched at use time), whose assurance is SECOND tier
// because the vault unlocks from a master password rather than a machine identity — which the tiered
// selector states out loud rather than implying parity.
//
// NATIVE GO, NO SUBPROCESS (the distroless constraint, INV-02): the Bitwarden crypto is implemented
// here over the standard library and golang.org/x/crypto — PBKDF2-SHA256 key derivation, the HKDF
// stretch to the (enc, mac) pair, and AES-256-CBC + HMAC-SHA256 cipher strings, MAC-verified before
// decryption in constant time. There is no `bw` CLI, no shell, and no temporary file: every secret
// lives only in memory for its single use.
//
// FAIL-CLOSED, READ-ONLY, INV-13: the master credentials are themselves SecretRefs (never literals),
// every failure direction — unreachable server, wrong password, unknown item, absent field, a cipher
// string whose MAC does not verify — returns an error and NEVER an empty or partial value. Nothing in
// this package writes to the vault.
//
// Provenance: [O] INV-02/INV-13/INV-17, spec/024 (REQ-2404, homelab tier).
package vaultwarden

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"

	"github.com/territory-grounder/grounder/core/config"
)

// Scheme is the SecretRef scheme this connector resolves.
const Scheme = "vw"

// SourceType is the vendor slug.
const SourceType = "vaultwarden"

// defaultTimeout bounds every call to the vault server; a hung backend must never stall a resolution.
const defaultTimeout = 15 * time.Second

// ErrRefused is the fail-closed sentinel every refusal wraps: an unreachable vault, a refused login, an
// unknown item, an absent field, or a cipher string that does not authenticate.
var ErrRefused = errors.New("vaultwarden: refused (fail closed)")

// Config constructs a Client. The two credentials are REFERENCES (INV-13): a vw: secret that
// authenticated from an inline literal would put the vault's own master password in a config artifact.
type Config struct {
	BaseURL     string           // e.g. https://vault.example.net
	EmailRef    config.SecretRef // the vault account's email
	PasswordRef config.SecretRef // the vault account's master password
	HTTPClient  *http.Client     // optional; nil ⇒ a bounded default
}

// Client is the read-only Vaultwarden client.
type Client struct {
	base   string
	email  config.SecretRef
	pass   config.SecretRef
	http   *http.Client
	cached *vaultKeys // populated on first successful unlock; in-memory only
}

// vaultKeys is the unlocked session: the API bearer and the user's symmetric (enc, mac) pair.
type vaultKeys struct {
	token string
	enc   []byte
	mac   []byte
}

// New builds a client. It validates configuration only — no network call and no secret resolution
// happens here, so a misconfigured deployment fails at construction rather than mid-triage.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("%w: base URL is required", ErrRefused)
	}
	if strings.TrimSpace(string(cfg.EmailRef)) == "" || strings.TrimSpace(string(cfg.PasswordRef)) == "" {
		return nil, fmt.Errorf("%w: both an email and a password REFERENCE are required", ErrRefused)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{base: base, email: cfg.EmailRef, pass: cfg.PasswordRef, http: hc}, nil
}

// RegisterResolver wires this client's vw: scheme into core/config so a `vw:<item>#<field>` reference
// resolves through it at use time. A nil client unregisters the scheme (fail closed).
// Composition-time only.
func RegisterResolver(c *Client) {
	if c == nil {
		config.RegisterSchemeResolver(Scheme, nil)
		return
	}
	config.RegisterSchemeResolver(Scheme, c.ResolveRef)
}

// ResolveRef dereferences `vw:<item name>#<field>` to a secret value. field is one of
// password | username | totp | notes, or the name of a custom field on the item. Fails closed on every
// error path — an unknown item and an absent field are refusals, never "".
func (c *Client) ResolveRef(ref string) (string, error) {
	_, rest, ok := strings.Cut(ref, ":")
	if !ok || rest == "" {
		return "", fmt.Errorf("%w: malformed secret reference %s", ErrRefused, config.RedactedRef(ref))
	}
	item, field, ok := strings.Cut(rest, "#")
	if !ok || strings.TrimSpace(item) == "" || strings.TrimSpace(field) == "" {
		return "", fmt.Errorf("%w: reference %s must be %s:<item>#<field>", ErrRefused, config.RedactedRef(ref), Scheme)
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	return c.value(ctx, strings.TrimSpace(item), strings.TrimSpace(field))
}

// value unlocks (once) and returns one decrypted field of one item.
func (c *Client) value(ctx context.Context, item, field string) (string, error) {
	keys, err := c.unlock(ctx)
	if err != nil {
		return "", err
	}
	ciphers, err := c.sync(ctx, keys)
	if err != nil {
		return "", err
	}
	for _, ci := range ciphers {
		name, derr := decryptString(ci.Name, keys.enc, keys.mac)
		if derr != nil || !strings.EqualFold(strings.TrimSpace(name), item) {
			continue
		}
		return itemField(ci, field, keys)
	}
	return "", fmt.Errorf("%w: no vault item named %q", ErrRefused, item)
}

// itemField selects and decrypts the requested field of a matched item.
func itemField(ci cipherItem, field string, keys *vaultKeys) (string, error) {
	var enc string
	switch strings.ToLower(field) {
	case "password":
		enc = ci.Login.Password
	case "username":
		enc = ci.Login.Username
	case "totp":
		enc = ci.Login.Totp
	case "notes":
		enc = ci.Notes
	default:
		for _, f := range ci.Fields {
			n, err := decryptString(f.Name, keys.enc, keys.mac)
			if err == nil && strings.EqualFold(strings.TrimSpace(n), field) {
				enc = f.Value
				break
			}
		}
	}
	if strings.TrimSpace(enc) == "" {
		return "", fmt.Errorf("%w: item carries no %q field", ErrRefused, field)
	}
	v, err := decryptString(enc, keys.enc, keys.mac)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", fmt.Errorf("%w: %q decrypted empty — refusing rather than returning a blank secret", ErrRefused, field)
	}
	return v, nil
}

// ---- the API surface (read-only) ---------------------------------------------------------------

type preloginResp struct {
	Kdf           int `json:"kdf"`
	KdfIterations int `json:"kdfIterations"`
}

type tokenResp struct {
	AccessToken string `json:"access_token"`
	Key         string `json:"Key"`
}

type cipherField struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

type cipherItem struct {
	Name  string `json:"Name"`
	Notes string `json:"Notes"`
	Login struct {
		Username string `json:"Username"`
		Password string `json:"Password"`
		Totp     string `json:"Totp"`
	} `json:"Login"`
	Fields []cipherField `json:"Fields"`
}

type syncResp struct {
	Ciphers []cipherItem `json:"Ciphers"`
}

// unlock resolves the master credentials, derives the keys, logs in, and decrypts the account's
// protected symmetric key. The derived material lives only in memory and is cached for the process.
func (c *Client) unlock(ctx context.Context) (*vaultKeys, error) {
	if c.cached != nil {
		return c.cached, nil
	}
	email, err := c.email.Resolve()
	if err != nil {
		return nil, fmt.Errorf("%w: resolve email reference: %v", ErrRefused, err)
	}
	password, err := c.pass.Resolve()
	if err != nil {
		return nil, fmt.Errorf("%w: resolve password reference: %v", ErrRefused, err)
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || password == "" {
		return nil, fmt.Errorf("%w: the email/password references resolved empty", ErrRefused)
	}

	var pre preloginResp
	if err := c.postJSON(ctx, "/identity/accounts/prelogin", map[string]string{"email": email}, "", &pre); err != nil {
		return nil, err
	}
	if pre.KdfIterations <= 0 {
		return nil, fmt.Errorf("%w: server declared no KDF iteration count", ErrRefused)
	}

	masterKey := pbkdf2.Key([]byte(password), []byte(email), pre.KdfIterations, 32, sha256.New)
	// The login hash is ONE further PBKDF2 iteration of the master key over the password — the server
	// never sees the master key itself, which is what keeps the vault decryptable only client-side.
	loginHash := base64.StdEncoding.EncodeToString(pbkdf2.Key(masterKey, []byte(password), 1, 32, sha256.New))

	form := url.Values{
		"grant_type":       {"password"},
		"username":         {email},
		"password":         {loginHash},
		"scope":            {"api offline_access"},
		"client_id":        {"cli"},
		"deviceType":       {"21"}, // SDK/other — this client is not a browser
		"deviceIdentifier": {"territory-grounder"},
		"deviceName":       {"territory-grounder"},
	}
	var tok tokenResp
	if err := c.postForm(ctx, "/identity/connect/token", form, &tok); err != nil {
		return nil, err
	}
	if strings.TrimSpace(tok.AccessToken) == "" || strings.TrimSpace(tok.Key) == "" {
		return nil, fmt.Errorf("%w: login returned no access token or no protected key", ErrRefused)
	}

	// KDF v2 (the current account format) stretches the master key into the (enc, mac) pair used to
	// unwrap the account's own symmetric key; the legacy v0 format uses the master key directly as enc
	// with the protected key carrying no MAC.
	stretchEnc, stretchMac := stretch(masterKey)
	userKey, err := decryptRaw(tok.Key, stretchEnc, stretchMac)
	if err != nil {
		return nil, fmt.Errorf("%w: the account's protected key did not decrypt (wrong password, or a KDF this build does not implement): %v", ErrRefused, err)
	}
	if len(userKey) != 64 {
		return nil, fmt.Errorf("%w: unwrapped symmetric key is %d bytes, want 64 (enc||mac)", ErrRefused, len(userKey))
	}
	c.cached = &vaultKeys{token: tok.AccessToken, enc: userKey[:32], mac: userKey[32:]}
	return c.cached, nil
}

// sync fetches the vault's ciphers (read-only).
func (c *Client) sync(ctx context.Context, keys *vaultKeys) ([]cipherItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/sync?excludeDomains=true", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	req.Header.Set("Authorization", "Bearer "+keys.token)
	var out syncResp
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out.Ciphers, nil
}

func (c *Client) postJSON(ctx context.Context, path string, body any, bearer string, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRefused, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRefused, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return c.do(req, out)
}

func (c *Client) postForm(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRefused, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req, out)
}

// do performs one request and decodes a JSON body on 2xx. A non-2xx NEVER yields a value: the status is
// reported without echoing the body, which on this API can carry credential material.
func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %v", ErrRefused, req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: %s %s: status %d", ErrRefused, req.Method, req.URL.Path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: %s %s: decode response: %v", ErrRefused, req.Method, req.URL.Path, err)
	}
	return nil
}

// ---- the crypto ---------------------------------------------------------------------------------

// stretch expands a 32-byte master key into the (enc, mac) pair via HKDF-Expand with the Bitwarden
// info labels. Exported behaviour is pinned by a known-answer test.
func stretch(masterKey []byte) (enc, mac []byte) {
	enc = make([]byte, 32)
	mac = make([]byte, 32)
	_, _ = io.ReadFull(hkdf.Expand(sha256.New, masterKey, []byte("enc")), enc)
	_, _ = io.ReadFull(hkdf.Expand(sha256.New, masterKey, []byte("mac")), mac)
	return enc, mac
}

// decryptString decrypts a Bitwarden cipher string to text.
func decryptString(s string, enc, mac []byte) (string, error) {
	b, err := decryptRaw(s, enc, mac)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decryptRaw parses and decrypts a Bitwarden cipher string: "<type>.<iv>|<ct>[|<mac>]", base64 parts.
// Types 2 (AesCbc256_HmacSha256_B64) and 0 (AesCbc256_B64) are supported; every other type is REFUSED
// rather than guessed at. For type 2 the HMAC is verified in constant time over iv||ct BEFORE any
// decryption — a tampered cipher string never reaches the block cipher.
func decryptRaw(s string, enc, mac []byte) ([]byte, error) {
	head, rest, ok := strings.Cut(strings.TrimSpace(s), ".")
	if !ok {
		return nil, fmt.Errorf("%w: cipher string carries no type prefix", ErrRefused)
	}
	typ, err := strconv.Atoi(head)
	if err != nil {
		return nil, fmt.Errorf("%w: cipher string type %q is not a number", ErrRefused, head)
	}
	parts := strings.Split(rest, "|")
	switch typ {
	case 2:
		if len(parts) != 3 {
			return nil, fmt.Errorf("%w: type 2 cipher string needs iv|ct|mac", ErrRefused)
		}
	case 0:
		if len(parts) != 2 {
			return nil, fmt.Errorf("%w: type 0 cipher string needs iv|ct", ErrRefused)
		}
	default:
		return nil, fmt.Errorf("%w: unsupported cipher string type %d", ErrRefused, typ)
	}
	iv, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: cipher string iv is not base64", ErrRefused)
	}
	ct, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: cipher string ciphertext is not base64", ErrRefused)
	}
	if typ == 2 {
		want, err := base64.StdEncoding.DecodeString(parts[2])
		if err != nil {
			return nil, fmt.Errorf("%w: cipher string mac is not base64", ErrRefused)
		}
		h := hmac.New(sha256.New, mac)
		h.Write(iv)
		h.Write(ct)
		if subtle.ConstantTimeCompare(h.Sum(nil), want) != 1 {
			return nil, fmt.Errorf("%w: cipher string MAC does not verify — tampered or wrong key", ErrRefused)
		}
	}
	if len(iv) != aes.BlockSize || len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("%w: cipher string block sizes are invalid", ErrRefused)
	}
	block, err := aes.NewCipher(enc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	return unpad(pt)
}

// unpad strips and VALIDATES PKCS#7 padding. An invalid pad is a refusal, never a best-effort slice:
// on a type-0 string (no MAC) the padding is the only integrity signal there is.
func unpad(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("%w: empty plaintext", ErrRefused)
	}
	n := int(b[len(b)-1])
	if n == 0 || n > aes.BlockSize || n > len(b) {
		return nil, fmt.Errorf("%w: invalid padding", ErrRefused)
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, fmt.Errorf("%w: invalid padding", ErrRefused)
		}
	}
	return b[:len(b)-n], nil
}

// WireDelivery is the composition-root entry point (spec/024 T-024-6): it registers the vw: scheme so a
// declared `vw:<item>#<field>` reference resolves from the homelab vault. Substrate OFF by default — an
// empty address is a logged no-op (vw: refs then fail closed at the config registry like any
// unregistered scheme, and every other scheme is unaffected). A configured-but-unbuildable client is an
// ERROR the caller refuses to start on: a declared vw: secret must never degrade to a plaintext
// fallback. transportWrap (optional) lets the root hand in its egress meter's Wrap so this client's
// traffic is metered like every other outbound call (TG-415).
func WireDelivery(addr string, emailRef, passwordRef string, logf func(string, ...any), transportWrap func(http.RoundTripper) http.RoundTripper) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if strings.TrimSpace(addr) == "" {
		RegisterResolver(nil)
		logf("vaultwarden: substrate OFF (TG_VAULTWARDEN_ADDR unset) — vw: references fail closed; no other scheme is affected")
		return nil
	}
	hc := &http.Client{Timeout: defaultTimeout}
	if transportWrap != nil {
		hc.Transport = transportWrap(http.DefaultTransport)
	}
	c, err := New(Config{
		BaseURL: addr, EmailRef: config.SecretRef(emailRef), PasswordRef: config.SecretRef(passwordRef),
		HTTPClient: hc,
	})
	if err != nil {
		return fmt.Errorf("vaultwarden delivery: %w", err)
	}
	RegisterResolver(c)
	logf("vaultwarden: vw: scheme REGISTERED against %s (homelab tier — the vault unlocks from a master password, not a machine identity; secrets resolve at use time, nothing at rest)", addr)
	return nil
}
