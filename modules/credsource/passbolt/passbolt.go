// Package passbolt is the Passbolt credential connector — the `passbolt:` SecretRef scheme (spec/024
// T-024-5, REQ-2406). Like the Vaultwarden connector it is a SECOND-TIER homelab backend: real secrets
// at rest, fetched at use time, but its irreducible on-host credential is the robot's OpenPGP PRIVATE
// KEY and passphrase — an unscopable identity that RELOCATES secret-zero rather than eliminating it
// (REQ-2408), which the tiered selector states out loud.
//
// NATIVE GO, NO SUBPROCESS (distroless, INV-02): the GPGAuth handshake and the resource decryption run
// over golang.org/x/crypto/openpgp in-process. There is no `gpg` binary, no keyring on disk, and no
// temporary file: the robot key is resolved from a SecretRef, decrypted in memory with its passphrase,
// and used for the single decryption it was fetched for.
//
// The OpenPGP implementation is github.com/ProtonMail/go-crypto/openpgp — the MAINTAINED fork, not
// golang.org/x/crypto/openpgp, which is frozen and documents itself as unsafe by design. This is a
// security path handling a robot identity, so the deprecated package was not an acceptable shortcut;
// the fork is a drop-in for the operations used here.
//
// FAIL-CLOSED, READ-ONLY, INV-13: the robot key and its passphrase are SecretRefs (never literals);
// every failure — unreachable server, a refused handshake, an unknown resource, an absent field, a
// message that does not decrypt — returns an error and never an empty or partial value. Nothing here
// writes to Passbolt: the connector performs the GPGAuth login and two GETs.
//
// Provenance: [O] INV-02/INV-13/INV-17, spec/024 (REQ-2406, homelab tier).
package passbolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgparmor "github.com/ProtonMail/go-crypto/openpgp/armor"

	"github.com/territory-grounder/grounder/core/config"
)

// Scheme is the SecretRef scheme this connector resolves.
const Scheme = "passbolt"

// SourceType is the vendor slug.
const SourceType = "passbolt"

// defaultTimeout bounds every call; a hung backend must never stall a resolution.
const defaultTimeout = 15 * time.Second

// ErrRefused is the fail-closed sentinel every refusal wraps.
var ErrRefused = errors.New("passbolt: refused (fail closed)")

// Config constructs a Client. Both credentials are REFERENCES (INV-13).
type Config struct {
	BaseURL       string           // e.g. https://passbolt.example.net
	PrivateKeyRef config.SecretRef // the robot's ASCII-armored OpenPGP private key
	PassphraseRef config.SecretRef // that key's passphrase
	HTTPClient    *http.Client     // optional; nil ⇒ a bounded default with a cookie jar
}

// Client is the read-only Passbolt client.
type Client struct {
	base     string
	keyRef   config.SecretRef
	passRef  config.SecretRef
	http     *http.Client
	session  *openpgp.Entity // the unlocked robot identity, in memory only
	loggedIn bool
}

// New builds a client. It validates configuration only — no network call and no secret resolution here,
// so a misconfigured deployment fails at construction rather than mid-triage.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("%w: base URL is required", ErrRefused)
	}
	if strings.TrimSpace(string(cfg.PrivateKeyRef)) == "" || strings.TrimSpace(string(cfg.PassphraseRef)) == "" {
		return nil, fmt.Errorf("%w: both a private-key and a passphrase REFERENCE are required", ErrRefused)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRefused, err)
		}
		hc = &http.Client{Timeout: defaultTimeout, Jar: jar}
	}
	if hc.Jar == nil {
		// The GPGAuth handshake authenticates the SESSION, so the cookie it sets is the credential for
		// every later read. A client with no jar would silently re-anonymize each request.
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRefused, err)
		}
		hc.Jar = jar
	}
	return &Client{base: base, keyRef: cfg.PrivateKeyRef, passRef: cfg.PassphraseRef, http: hc}, nil
}

// RegisterResolver wires the passbolt: scheme into core/config. A nil client unregisters it (fail
// closed). Composition-time only.
func RegisterResolver(c *Client) {
	if c == nil {
		config.RegisterSchemeResolver(Scheme, nil)
		return
	}
	config.RegisterSchemeResolver(Scheme, c.ResolveRef)
}

// ResolveRef dereferences `passbolt:<resource name>#<field>` to a secret value. field is one of
// password | description (both from the encrypted secret) or username | uri (resource metadata).
func (c *Client) ResolveRef(ref string) (string, error) {
	_, rest, ok := strings.Cut(ref, ":")
	if !ok || rest == "" {
		return "", fmt.Errorf("%w: malformed secret reference %s", ErrRefused, config.RedactedRef(ref))
	}
	name, field, ok := strings.Cut(rest, "#")
	if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(field) == "" {
		return "", fmt.Errorf("%w: reference %s must be %s:<resource>#<field>", ErrRefused, config.RedactedRef(ref), Scheme)
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	return c.value(ctx, strings.TrimSpace(name), strings.TrimSpace(field))
}

// value logs in (once), finds the named resource, and returns the requested field.
func (c *Client) value(ctx context.Context, name, field string) (string, error) {
	if err := c.login(ctx); err != nil {
		return "", err
	}
	res, err := c.resources(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range res {
		if !strings.EqualFold(strings.TrimSpace(r.Name), name) {
			continue
		}
		switch strings.ToLower(field) {
		case "username":
			if strings.TrimSpace(r.Username) == "" {
				return "", fmt.Errorf("%w: resource %q carries no username", ErrRefused, name)
			}
			return r.Username, nil
		case "uri":
			if strings.TrimSpace(r.URI) == "" {
				return "", fmt.Errorf("%w: resource %q carries no uri", ErrRefused, name)
			}
			return r.URI, nil
		case "password", "description":
			return c.secretField(ctx, r.ID, field)
		default:
			return "", fmt.Errorf("%w: unknown field %q (want password|description|username|uri)", ErrRefused, field)
		}
	}
	return "", fmt.Errorf("%w: no resource named %q", ErrRefused, name)
}

// secretField fetches and decrypts the resource's secret, then selects the field. Passbolt stores either
// a bare password string or a JSON object with password/description; both are handled, and anything else
// is a refusal rather than a guess.
func (c *Client) secretField(ctx context.Context, resourceID, field string) (string, error) {
	var out struct {
		Body struct {
			Data string `json:"data"`
		} `json:"body"`
	}
	if err := c.getJSON(ctx, "/secrets/resource/"+url.PathEscape(resourceID)+".json?api-version=v2", &out); err != nil {
		return "", err
	}
	plain, err := c.decrypt(out.Body.Data)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(plain)
	if strings.HasPrefix(trimmed, "{") {
		var obj struct {
			Password    string `json:"password"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
			return "", fmt.Errorf("%w: the decrypted secret is neither a bare password nor a known object", ErrRefused)
		}
		v := obj.Password
		if strings.EqualFold(field, "description") {
			v = obj.Description
		}
		if strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("%w: the decrypted secret carries no %q", ErrRefused, field)
		}
		return v, nil
	}
	if strings.EqualFold(field, "description") {
		return "", fmt.Errorf("%w: this resource stores a bare password and carries no description", ErrRefused)
	}
	if trimmed == "" {
		return "", fmt.Errorf("%w: the secret decrypted empty — refusing rather than returning a blank", ErrRefused)
	}
	return plain, nil
}

// ---- GPGAuth ------------------------------------------------------------------------------------

// login performs the two-stage GPGAuth handshake: the server encrypts a nonce to the robot's public key,
// the client proves possession by decrypting it and returning the token verbatim. The session cookie the
// second stage sets is the credential for every later read.
func (c *Client) login(ctx context.Context) error {
	if c.loggedIn {
		return nil
	}
	entity, err := c.unlockKey()
	if err != nil {
		return err
	}
	fp := fmt.Sprintf("%X", entity.PrimaryKey.Fingerprint)

	// Stage 1 — ask for the challenge. The encrypted token arrives in a HEADER, URL-encoded.
	resp, err := c.postJSON(ctx, "/auth/login.json?api-version=v2", loginBody(fp, ""))
	if err != nil {
		return err
	}
	raw := resp.Header.Get("X-GPGAuth-User-Auth-Token")
	resp.Body.Close()
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%w: the server issued no GPGAuth challenge (is this robot's public key imported?)", ErrRefused)
	}
	unescaped, err := url.QueryUnescape(raw)
	if err != nil {
		return fmt.Errorf("%w: the GPGAuth challenge was not decodable", ErrRefused)
	}
	// Passbolt escapes the armored block's newlines; restore them before parsing.
	unescaped = strings.ReplaceAll(unescaped, `\ `, " ")
	unescaped = strings.ReplaceAll(unescaped, `\+`, "+")

	token, err := c.decrypt(unescaped)
	if err != nil {
		return fmt.Errorf("%w: the GPGAuth challenge did not decrypt with this robot key: %v", ErrRefused, err)
	}

	// Stage 2 — return the proven token. A non-2xx here is a refused login, never a partial session.
	resp2, err := c.postJSON(ctx, "/auth/login.json?api-version=v2", loginBody(fp, strings.TrimSpace(token)))
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode < 200 || resp2.StatusCode >= 300 {
		return fmt.Errorf("%w: GPGAuth stage 2 refused (status %d)", ErrRefused, resp2.StatusCode)
	}
	c.loggedIn = true
	return nil
}

func loginBody(keyid, token string) map[string]any {
	auth := map[string]string{"keyid": keyid}
	if token != "" {
		auth["user_token_result"] = token
	}
	return map[string]any{"data": map[string]any{"gpg_auth": auth}}
}

// unlockKey resolves the robot's private key and passphrase and decrypts the key in memory.
func (c *Client) unlockKey() (*openpgp.Entity, error) {
	if c.session != nil {
		return c.session, nil
	}
	armored, err := c.keyRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("%w: resolve private-key reference: %v", ErrRefused, err)
	}
	passphrase, err := c.passRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("%w: resolve passphrase reference: %v", ErrRefused, err)
	}
	ring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil || len(ring) == 0 {
		return nil, fmt.Errorf("%w: the robot private key did not parse as an armored OpenPGP key", ErrRefused)
	}
	e := ring[0]
	if e.PrivateKey == nil {
		return nil, fmt.Errorf("%w: the configured key carries no private material (a public key cannot decrypt)", ErrRefused)
	}
	if e.PrivateKey.Encrypted {
		if err := e.PrivateKey.Decrypt([]byte(passphrase)); err != nil {
			return nil, fmt.Errorf("%w: the robot private key did not unlock with the configured passphrase", ErrRefused)
		}
	}
	for _, sub := range e.Subkeys {
		if sub.PrivateKey != nil && sub.PrivateKey.Encrypted {
			_ = sub.PrivateKey.Decrypt([]byte(passphrase)) // a subkey that will not unlock simply cannot decrypt
		}
	}
	c.session = e
	return e, nil
}

// decrypt opens an ASCII-armored PGP message addressed to the robot key.
func (c *Client) decrypt(armored string) (string, error) {
	if strings.TrimSpace(armored) == "" {
		return "", fmt.Errorf("%w: empty PGP message", ErrRefused)
	}
	e, err := c.unlockKey()
	if err != nil {
		return "", err
	}
	block, err := armor(armored)
	if err != nil {
		return "", err
	}
	md, err := openpgp.ReadMessage(block, openpgp.EntityList{e}, nil, nil)
	if err != nil {
		return "", fmt.Errorf("%w: the message did not open with this robot key: %v", ErrRefused, err)
	}
	body, err := io.ReadAll(io.LimitReader(md.UnverifiedBody, 1<<20))
	if err != nil {
		return "", fmt.Errorf("%w: reading the decrypted body failed: %v", ErrRefused, err)
	}
	return string(body), nil
}

// ---- transport ----------------------------------------------------------------------------------

func (c *Client) postJSON(ctx context.Context, path string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: POST %s: %v", ErrRefused, path, err)
	}
	return resp, nil
}

type resource struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
	URI      string `json:"uri"`
}

func (c *Client) resources(ctx context.Context) ([]resource, error) {
	var out struct {
		Body []resource `json:"body"`
	}
	if err := c.getJSON(ctx, "/resources.json?api-version=v2", &out); err != nil {
		return nil, err
	}
	return out.Body, nil
}

// getJSON performs one authenticated GET. A non-2xx NEVER yields a value, and the body is not echoed —
// on this API it can carry secret material.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRefused, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: GET %s: %v", ErrRefused, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: GET %s: status %d", ErrRefused, path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: GET %s: decode response: %v", ErrRefused, path, err)
	}
	return nil
}

// armor decodes an ASCII-armored PGP block into its binary body.
func armor(s string) (io.Reader, error) {
	b, err := pgparmor.Decode(strings.NewReader(s))
	if err != nil {
		return nil, fmt.Errorf("%w: the PGP message is not valid ASCII armor: %v", ErrRefused, err)
	}
	return b.Body, nil
}

// WireDelivery is the composition-root entry point: it registers the passbolt: scheme. An empty address
// is a logged no-op (substrate OFF ⇒ passbolt: refs fail closed, every other scheme unaffected); a
// configured-but-unbuildable client is an ERROR the caller refuses to start on.
func WireDelivery(addr, keyRef, passphraseRef string, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if strings.TrimSpace(addr) == "" {
		RegisterResolver(nil)
		logf("passbolt: substrate OFF (TG_PASSBOLT_ADDR unset) — passbolt: references fail closed; no other scheme is affected")
		return nil
	}
	c, err := New(Config{BaseURL: addr, PrivateKeyRef: config.SecretRef(keyRef), PassphraseRef: config.SecretRef(passphraseRef)})
	if err != nil {
		return fmt.Errorf("passbolt delivery: %w", err)
	}
	RegisterResolver(c)
	logf("passbolt: passbolt: scheme REGISTERED against %s (homelab tier — the robot's OpenPGP private key is the irreducible on-host credential; no leased or individually-revocable secrets)", addr)
	return nil
}
