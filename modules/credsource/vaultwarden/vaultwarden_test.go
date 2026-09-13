package vaultwarden

// spec/024 T-024-6 — the vw: resolver's oracles. The claims:
//
//   1. The key derivation is STANDARD, not this file's invention: PBKDF2-HMAC-SHA256 matches a published
//      known-answer vector, so a vault created by any Bitwarden client unlocks here.
//   2. Cipher strings authenticate BEFORE they decrypt: a flipped ciphertext/IV/MAC byte, a wrong key,
//      an unsupported type, a malformed part, and invalid PKCS#7 padding each REFUSE — never a partial
//      or empty "secret".
//   3. End to end over a fake server: prelogin → login → sync → the requested field decrypts, with the
//      master password never leaving as plaintext (the server sees only the login hash).
//   4. Reference grammar and fail-closed selection: a malformed ref, an unknown item, and an absent
//      field all refuse; the refusal never echoes the reference's payload (REQ-2205).

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/pbkdf2"

	"github.com/territory-grounder/grounder/core/config"
)

// Claim 1 — the published PBKDF2-HMAC-SHA256 known-answer vector (password/salt, 1 iteration, 32
// bytes). If this ever drifts, every vault in the world stops unlocking here; a round-trip test against
// our own encryptor could not catch that, which is why the vector is pinned rather than self-generated.
func TestKeyDerivationMatchesThePublishedVector(t *testing.T) {
	got := hex.EncodeToString(pbkdf2.Key([]byte("password"), []byte("salt"), 1, 32, sha256.New))
	const want = "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if got != want {
		t.Fatalf("PBKDF2-HMAC-SHA256 vector mismatch:\n got %s\nwant %s", got, want)
	}
	// The HKDF stretch must produce two DISTINCT 32-byte keys from one master key — reusing one key for
	// both encryption and authentication is the classic break this labelled expansion exists to avoid.
	enc, mac := stretch(pbkdf2.Key([]byte("pw"), []byte("user@example.net"), 100, 32, sha256.New))
	if len(enc) != 32 || len(mac) != 32 {
		t.Fatalf("stretch must yield two 32-byte keys, got %d/%d", len(enc), len(mac))
	}
	if string(enc) == string(mac) {
		t.Fatal("the enc and mac keys must differ — one key for both roles is a break, not a shortcut")
	}
}

// encryptFixture builds a type-2 cipher string the way a Bitwarden client would, so the decrypt path is
// exercised over genuinely-shaped input. Fixture-only.
func encryptFixture(t *testing.T, plaintext string, enc, mac []byte) string {
	t.Helper()
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(enc)
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte(plaintext)
	pad := aes.BlockSize - len(pt)%aes.BlockSize
	for i := 0; i < pad; i++ {
		pt = append(pt, byte(pad))
	}
	ct := make([]byte, len(pt))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, pt)
	h := hmac.New(sha256.New, mac)
	h.Write(iv)
	h.Write(ct)
	return "2." + base64.StdEncoding.EncodeToString(iv) + "|" +
		base64.StdEncoding.EncodeToString(ct) + "|" +
		base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func testKeys(t *testing.T) (enc, mac []byte) {
	t.Helper()
	return stretch(pbkdf2.Key([]byte("master-pw"), []byte("op@example.net"), 200, 32, sha256.New))
}

// Claim 2 — authenticate before decrypting, and refuse every malformed direction.
func TestCipherStringsAuthenticateBeforeTheyDecrypt(t *testing.T) {
	enc, mac := testKeys(t)
	good := encryptFixture(t, "s3cr3t-value", enc, mac)

	if got, err := decryptString(good, enc, mac); err != nil || got != "s3cr3t-value" {
		t.Fatalf("a well-formed cipher string must decrypt: got %q err=%v", got, err)
	}

	parts := strings.SplitN(strings.TrimPrefix(good, "2."), "|", 3)
	flip := func(b64 string) string {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatal(err)
		}
		raw[0] ^= 0x01
		return base64.StdEncoding.EncodeToString(raw)
	}
	tampered := map[string]string{
		"flipped ciphertext": "2." + parts[0] + "|" + flip(parts[1]) + "|" + parts[2],
		"flipped iv":         "2." + flip(parts[0]) + "|" + parts[1] + "|" + parts[2],
		"flipped mac":        "2." + parts[0] + "|" + parts[1] + "|" + flip(parts[2]),
	}
	for name, ct := range tampered {
		if _, err := decryptString(ct, enc, mac); err == nil {
			t.Errorf("%s must refuse — a tampered cipher string may never decrypt", name)
		} else if !errors.Is(err, ErrRefused) {
			t.Errorf("%s: refusal must wrap ErrRefused, got %v", name, err)
		}
	}

	// A wrong key must refuse at the MAC, never produce garbage plaintext.
	otherEnc, otherMac := stretch(pbkdf2.Key([]byte("other-pw"), []byte("op@example.net"), 200, 32, sha256.New))
	if _, err := decryptString(good, otherEnc, otherMac); err == nil {
		t.Error("a wrong key must refuse")
	}

	malformed := []string{
		"", "no-type-prefix", "9." + parts[0] + "|" + parts[1] + "|" + parts[2], // unsupported type
		"2." + parts[0] + "|" + parts[1],     // type 2 missing its mac
		"0." + parts[0],                      // type 0 missing its ct
		"2.!!!|" + parts[1] + "|" + parts[2], // iv not base64
	}
	for _, s := range malformed {
		if _, err := decryptString(s, enc, mac); err == nil {
			t.Errorf("malformed cipher string %q must refuse", s)
		}
	}

	// Invalid PKCS#7 padding refuses — on a type-0 string it is the only integrity signal there is.
	if _, err := unpad([]byte{1, 2, 3, 99}); err == nil {
		t.Error("invalid padding must refuse")
	}
	if _, err := unpad(nil); err == nil {
		t.Error("empty plaintext must refuse")
	}
	if out, err := unpad([]byte{'h', 'i', 2, 2}); err != nil || string(out) != "hi" {
		t.Errorf("valid padding must strip exactly: %q %v", out, err)
	}
}

// fakeVault serves the three read endpoints with a vault whose items are encrypted under keys derived
// from the fixture password — so the end-to-end test exercises the REAL derivation, not a stub.
func fakeVault(t *testing.T, email, password string, iterations int) (*httptest.Server, []byte, []byte) {
	t.Helper()
	master := pbkdf2.Key([]byte(password), []byte(email), iterations, 32, sha256.New)
	stretchEnc, stretchMac := stretch(master)
	// The account's own symmetric key (enc||mac), wrapped under the stretched master key.
	userKey := make([]byte, 64)
	if _, err := rand.Read(userKey); err != nil {
		t.Fatal(err)
	}
	uEnc, uMac := userKey[:32], userKey[32:]
	protected := encryptRawFixture(t, userKey, stretchEnc, stretchMac)
	wantLoginHash := base64.StdEncoding.EncodeToString(pbkdf2.Key(master, []byte(password), 1, 32, sha256.New))

	mux := http.NewServeMux()
	mux.HandleFunc("/identity/accounts/prelogin", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(preloginResp{Kdf: 0, KdfIterations: iterations})
	})
	mux.HandleFunc("/identity/connect/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		// THE MASTER PASSWORD MUST NEVER ARRIVE: the server sees only the derived login hash.
		if r.Form.Get("password") == password {
			t.Error("the master password reached the server in plaintext — the login hash is the whole point")
		}
		if r.Form.Get("password") != wantLoginHash {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(tokenResp{AccessToken: "tok-1", Key: protected})
	})
	mux.HandleFunc("/api/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		item := cipherItem{
			Name:  encryptFixture(t, "nms-api", uEnc, uMac),
			Notes: encryptFixture(t, "the note", uEnc, uMac),
			Fields: []cipherField{{
				Name:  encryptFixture(t, "site-token", uEnc, uMac),
				Value: encryptFixture(t, "custom-value", uEnc, uMac),
			}},
		}
		item.Login.Username = encryptFixture(t, "tg-reader", uEnc, uMac)
		item.Login.Password = encryptFixture(t, "p4ssw0rd", uEnc, uMac)
		_ = json.NewEncoder(w).Encode(syncResp{Ciphers: []cipherItem{item}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, uEnc, uMac
}

// encryptRawFixture is encryptFixture over raw bytes (the protected symmetric key).
func encryptRawFixture(t *testing.T, raw, enc, mac []byte) string {
	t.Helper()
	return encryptFixture(t, string(raw), enc, mac)
}

// Claim 3 + 4 — end to end, and every selection failure refuses.
func TestResolveRefEndToEndAndFailClosed(t *testing.T) {
	const email, password = "op@example.net", "master-pw"
	srv, _, _ := fakeVault(t, email, password, 600000)
	t.Setenv("VW_EMAIL", email)
	t.Setenv("VW_PASSWORD", password)

	newClient := func(t *testing.T) *Client {
		t.Helper()
		c, err := New(Config{
			BaseURL: srv.URL, EmailRef: "env:VW_EMAIL", PasswordRef: "env:VW_PASSWORD",
			HTTPClient: srv.Client(),
		})
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		return c
	}

	for ref, want := range map[string]string{
		"vw:nms-api#password":   "p4ssw0rd",
		"vw:nms-api#username":   "tg-reader",
		"vw:nms-api#notes":      "the note",
		"vw:nms-api#site-token": "custom-value", // a custom field, matched by its decrypted name
	} {
		got, err := newClient(t).ResolveRef(ref)
		if err != nil || got != want {
			t.Errorf("%s = %q err=%v, want %q", ref, got, err, want)
		}
	}

	// Fail-closed selection: unknown item, absent field, malformed grammar.
	for _, ref := range []string{"vw:no-such-item#password", "vw:nms-api#totp", "vw:nms-api", "vw:#password", "vw:nms-api#"} {
		if v, err := newClient(t).ResolveRef(ref); err == nil {
			t.Errorf("%s must refuse, got %q", ref, v)
		} else if !errors.Is(err, ErrRefused) {
			t.Errorf("%s: refusal must wrap ErrRefused, got %v", ref, err)
		}
	}

	// REQ-2205: a malformed reference may be a pasted literal — the refusal must never echo it.
	const canary = "hunter2-canary"
	if _, err := newClient(t).ResolveRef(canary); err == nil {
		t.Error("a schemeless reference must refuse")
	} else if strings.Contains(err.Error(), canary) {
		t.Errorf("the refusal echoed the payload: %v", err)
	}

	// A wrong master password fails at the protected-key unwrap — never a blank secret.
	t.Setenv("VW_PASSWORD", "wrong-pw")
	if v, err := newClient(t).ResolveRef("vw:nms-api#password"); err == nil {
		t.Errorf("a wrong master password must refuse, got %q", v)
	}
}

// Construction validates configuration before any network call, and the scheme registration is
// nil-safe (a nil client unregisters, fail closed).
func TestNewValidatesAndRegistrationIsNilSafe(t *testing.T) {
	for _, cfg := range []Config{
		{EmailRef: "env:A", PasswordRef: "env:B"},                    // no base URL
		{BaseURL: "https://v", PasswordRef: "env:B"},                 // no email ref
		{BaseURL: "https://v", EmailRef: "env:A"},                    // no password ref
		{BaseURL: "https://v", EmailRef: "  ", PasswordRef: "env:B"}, // blank ref
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("misconfigured client must refuse at construction: %+v", cfg)
		}
	}
	RegisterResolver(nil)
	if _, err := config.SecretRef("vw:x#y").Resolve(); err == nil {
		t.Error("an unregistered vw: scheme must fail closed")
	}
}
