package passbolt

// spec/024 T-024-5 — the passbolt: resolver's oracles. The claims:
//
//   1. The GPGAuth handshake is a real proof of possession: the fake server encrypts a nonce to the
//      robot's PUBLIC key and accepts stage 2 only if the exact plaintext comes back — so a client that
//      could not decrypt cannot log in, and a wrong key fails the handshake rather than the read.
//   2. End to end: login → resources → secret → the requested field decrypts, for both storage shapes
//      Passbolt uses (a bare password string and the JSON object with password/description).
//   3. Fail-closed everywhere: an unknown resource, an unknown field, a description asked of a
//      bare-password resource, a locked key with the wrong passphrase, a public-key-only configuration,
//      and a malformed reference all refuse — and the refusal never echoes the reference payload.
//   4. Metadata fields (username/uri) come from the resource record, and their absence is a refusal
//      rather than an empty string.

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgparmor "github.com/ProtonMail/go-crypto/openpgp/armor"
)

// robotKey generates a real OpenPGP identity and returns (armored private key, the entity). Generating
// rather than pinning keeps the test hermetic while still exercising the genuine library path.
func robotKey(t *testing.T) (string, *openpgp.Entity) {
	t.Helper()
	e, err := openpgp.NewEntity("tg-robot", "territory-grounder", "robot@example.net", nil)
	if err != nil {
		t.Fatalf("generate robot key: %v", err)
	}
	var buf bytes.Buffer
	w, err := pgparmor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return buf.String(), e
}

// encryptTo produces an ASCII-armored message readable only by the given entity.
func encryptTo(t *testing.T, e *openpgp.Entity, plaintext string) string {
	t.Helper()
	var buf bytes.Buffer
	aw, err := pgparmor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := openpgp.Encrypt(aw, openpgp.EntityList{e}, nil, nil, nil)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := w.Write([]byte(plaintext)); err != nil {
		t.Fatal(err)
	}
	w.Close()
	aw.Close()
	return buf.String()
}

// fakePassbolt serves the GPGAuth handshake and the two read endpoints. It PROVES possession: stage 2 is
// accepted only when the client returns the exact nonce the server encrypted to the robot's public key.
func fakePassbolt(t *testing.T, e *openpgp.Entity, secretPlaintext string) *httptest.Server {
	t.Helper()
	nonceRaw := make([]byte, 16)
	if _, err := rand.Read(nonceRaw); err != nil {
		t.Fatal(err)
	}
	nonce := fmt.Sprintf("gpgauthv1.3.0|36|%x|gpgauthv1.3.0", nonceRaw)

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login.json", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Data struct {
				GPGAuth struct {
					KeyID           string `json:"keyid"`
					UserTokenResult string `json:"user_token_result"`
				} `json:"gpg_auth"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Data.GPGAuth.UserTokenResult == "" {
			// Stage 1: hand back the challenge, URL-encoded in the header as Passbolt does.
			w.Header().Set("X-GPGAuth-User-Auth-Token", url.QueryEscape(encryptTo(t, e, nonce)))
			w.WriteHeader(http.StatusOK)
			return
		}
		// Stage 2: the token must match EXACTLY — this is the proof of possession.
		if body.Data.GPGAuth.UserTokenResult != nonce {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "passbolt_session", Value: "sess-1", Path: "/"})
		w.Header().Set("X-GPGAuth-Progress", "complete")
		w.WriteHeader(http.StatusOK)
	})
	authed := func(r *http.Request) bool {
		c, err := r.Cookie("passbolt_session")
		return err == nil && c.Value == "sess-1"
	}
	mux.HandleFunc("/resources.json", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"body": []resource{
			{ID: "res-1", Name: "nms-api", Username: "tg-reader", URI: "https://nms.example.net"},
			{ID: "res-2", Name: "bare-only"},
		}})
	})
	mux.HandleFunc("/secrets/resource/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		plain := secretPlaintext
		if strings.Contains(r.URL.Path, "res-2") {
			plain = "bare-password-value"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"body": map[string]string{"data": encryptTo(t, e, plain)}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func clientFor(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(Config{
		BaseURL: srv.URL, PrivateKeyRef: "env:PB_KEY", PassphraseRef: "env:PB_PASS",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

func TestGPGAuthProvesPossessionAndResolvesEndToEnd(t *testing.T) {
	armored, e := robotKey(t)
	t.Setenv("PB_KEY", armored)
	t.Setenv("PB_PASS", "") // an unencrypted generated key: the passphrase is unused, the REF still required
	srv := fakePassbolt(t, e, `{"password":"p4ssw0rd","description":"the note"}`)

	for ref, want := range map[string]string{
		"passbolt:nms-api#password":    "p4ssw0rd",
		"passbolt:nms-api#description": "the note",
		"passbolt:nms-api#username":    "tg-reader",
		"passbolt:nms-api#uri":         "https://nms.example.net",
		"passbolt:bare-only#password":  "bare-password-value", // the legacy bare-string shape
	} {
		got, err := clientFor(t, srv).ResolveRef(ref)
		if err != nil || got != want {
			t.Errorf("%s = %q err=%v, want %q", ref, got, err, want)
		}
	}
}

func TestAWrongRobotKeyCannotCompleteTheHandshake(t *testing.T) {
	_, serverEntity := robotKey(t)
	otherArmored, _ := robotKey(t) // a DIFFERENT identity
	t.Setenv("PB_KEY", otherArmored)
	t.Setenv("PB_PASS", "")
	srv := fakePassbolt(t, serverEntity, `{"password":"p"}`)

	_, err := clientFor(t, srv).ResolveRef("passbolt:nms-api#password")
	if err == nil {
		t.Fatal("a client holding the wrong key must not authenticate")
	}
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "challenge did not decrypt") {
		t.Fatalf("the refusal must name the failed handshake, got %v", err)
	}
}

func TestFailClosedDirections(t *testing.T) {
	armored, e := robotKey(t)
	t.Setenv("PB_KEY", armored)
	t.Setenv("PB_PASS", "")
	srv := fakePassbolt(t, e, `{"password":"p4ssw0rd"}`)

	for _, ref := range []string{
		"passbolt:no-such-resource#password", // unknown resource
		"passbolt:nms-api#totp",              // unknown field
		"passbolt:bare-only#description",     // a bare-password resource has no description
		"passbolt:bare-only#username",        // metadata absent on that record
		"passbolt:nms-api",                   // no field
		"passbolt:#password",                 // no resource
	} {
		if v, err := clientFor(t, srv).ResolveRef(ref); err == nil {
			t.Errorf("%s must refuse, got %q", ref, v)
		} else if !errors.Is(err, ErrRefused) {
			t.Errorf("%s: refusal must wrap ErrRefused, got %v", ref, err)
		}
	}

	// REQ-2205: a malformed reference may be a pasted literal — never echo it.
	const canary = "hunter2-canary"
	if _, err := clientFor(t, srv).ResolveRef(canary); err == nil {
		t.Error("a schemeless reference must refuse")
	} else if strings.Contains(err.Error(), canary) {
		t.Errorf("the refusal echoed the payload: %v", err)
	}

	// A PUBLIC key alone cannot decrypt: the configuration is refused rather than failing later.
	var pub bytes.Buffer
	w, err := pgparmor.Encode(&pub, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	w.Close()
	t.Setenv("PB_KEY", pub.String())
	if _, err := clientFor(t, srv).ResolveRef("passbolt:nms-api#password"); err == nil ||
		!strings.Contains(err.Error(), "no private material") {
		t.Errorf("a public-key-only configuration must refuse with that reason, got %v", err)
	}

	// A key reference that does not resolve at all is a refusal, not an empty identity.
	t.Setenv("PB_KEY", "not-an-armored-key")
	if _, err := clientFor(t, srv).ResolveRef("passbolt:nms-api#password"); err == nil {
		t.Error("an unparseable robot key must refuse")
	}
}

func TestNewValidatesAndRegistrationIsNilSafe(t *testing.T) {
	for _, cfg := range []Config{
		{PrivateKeyRef: "env:A", PassphraseRef: "env:B"},                   // no base URL
		{BaseURL: "https://p", PassphraseRef: "env:B"},                     // no key ref
		{BaseURL: "https://p", PrivateKeyRef: "env:A"},                     // no passphrase ref
		{BaseURL: "https://p", PrivateKeyRef: " ", PassphraseRef: "env:B"}, // blank ref
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("misconfigured client must refuse at construction: %+v", cfg)
		}
	}
	// The substrate-OFF path unregisters the scheme; a passbolt: ref then fails closed.
	if err := WireDelivery("", "", "", nil); err != nil {
		t.Fatalf("an unset address must be a no-op, got %v", err)
	}
	// A configured-but-unbuildable client is an ERROR the caller refuses to start on.
	if err := WireDelivery("https://p", "", "", nil); err == nil {
		t.Error("a configured backend with no key reference must error rather than register")
	}
}
