package seal

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func mustMaster(t *testing.T) []byte {
	t.Helper()
	m := make([]byte, KeySize)
	for i := range m {
		m[i] = byte(i + 1)
	}
	return m
}

// The Sealer with a local wrapper round-trips, and is INTEROPERABLE with the original seal.Seal/Open — a blob
// sealed one way opens the other — so introducing the abstraction changes no persisted format or crypto.
func TestSealerLocalRoundTripAndInterop(t *testing.T) {
	master := mustMaster(t)
	w, err := NewLocalWrapper(master)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSealer(w)
	if err != nil {
		t.Fatal(err)
	}
	val := []byte("librenms.push.token=s3cr3t")

	// Sealer → Sealer
	sealed, err := s.Seal("librenms.push", val)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := s.Open("librenms.push", sealed)
	if err != nil || !bytes.Equal(got, val) {
		t.Fatalf("open: %v got=%q", err, got)
	}
	// wrong name must fail closed
	if _, err := s.Open("other.name", sealed); err != ErrOpenFailed {
		t.Fatalf("wrong-name open must fail closed, got %v", err)
	}
	// tamper must fail closed
	bad := sealed
	bad.Ciphertext = append([]byte(nil), sealed.Ciphertext...)
	bad.Ciphertext[0] ^= 0xff
	if _, err := s.Open("librenms.push", bad); err != ErrOpenFailed {
		t.Fatalf("tampered open must fail closed, got %v", err)
	}

	// Interop: seal.Seal → Sealer.Open, and Sealer.Seal → seal.Open (same crypto, same format).
	legacy, err := Seal(master, "librenms.push", val)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Open("librenms.push", legacy); err != nil || !bytes.Equal(got, val) {
		t.Fatalf("Sealer must open a seal.Seal blob: %v got=%q", err, got)
	}
	if got, err := Open(master, "librenms.push", sealed); err != nil || !bytes.Equal(got, val) {
		t.Fatalf("seal.Open must open a Sealer blob: %v got=%q", err, got)
	}
}

func TestNewSealerRejectsNilWrapper(t *testing.T) {
	if _, err := NewSealer(nil); err == nil {
		t.Fatal("nil wrapper must fail closed")
	}
}

// fakeTransit simulates OpenBao Transit: encrypt wraps base64(DEK) into "faketransit:v1:<b64>", decrypt reverses
// it. It records the paths hit so the test proves the wrapper targets /v1/transit/{encrypt,decrypt}/<key>.
type fakeTransit struct{ paths []string }

func (f *fakeTransit) Do(req *http.Request) (*http.Response, error) {
	f.paths = append(f.paths, req.URL.Path)
	var in map[string]string
	_ = json.NewDecoder(req.Body).Decode(&in)
	var out map[string]map[string]string
	switch {
	case strings.Contains(req.URL.Path, "/encrypt/"):
		out = map[string]map[string]string{"data": {"ciphertext": "faketransit:v1:" + in["plaintext"]}}
	case strings.Contains(req.URL.Path, "/decrypt/"):
		out = map[string]map[string]string{"data": {"plaintext": strings.TrimPrefix(in["ciphertext"], "faketransit:v1:")}}
	default:
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}
	b, _ := json.Marshal(out)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: make(http.Header)}, nil
}

func TestSealerTransitRoundTrip(t *testing.T) {
	t.Setenv("TG_TEST_BAO_TOKEN", "s.fake")
	fake := &fakeTransit{}
	w, err := NewTransitWrapper(TransitConfig{
		BaseURL: "https://openbao.example:8200", KeyName: "tg-seal",
		TokenRef: "env:TG_TEST_BAO_TOKEN", HTTP: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSealer(w)
	if err != nil {
		t.Fatal(err)
	}
	val := []byte("master-key-never-on-the-worker")
	sealed, err := s.Seal("cosign.pass", val)
	if err != nil {
		t.Fatalf("transit seal: %v", err)
	}
	// The wrapped DEK is Transit's ciphertext, not a local AES-GCM blob.
	if !strings.HasPrefix(string(sealed.WrappedDEK), "faketransit:v1:") {
		t.Fatalf("wrapped DEK should be the Transit ciphertext, got %q", sealed.WrappedDEK)
	}
	got, err := s.Open("cosign.pass", sealed)
	if err != nil || !bytes.Equal(got, val) {
		t.Fatalf("transit open: %v got=%q", err, got)
	}
	// it actually hit the transit encrypt+decrypt endpoints for key tg-seal
	if len(fake.paths) != 2 || !strings.HasSuffix(fake.paths[0], "/v1/transit/encrypt/tg-seal") || !strings.HasSuffix(fake.paths[1], "/v1/transit/decrypt/tg-seal") {
		t.Fatalf("unexpected transit paths: %v", fake.paths)
	}
	// a wrapped DEK the substrate refuses to decrypt fails closed.
	bad := sealed
	bad.WrappedDEK = []byte("faketransit:v1:" + base64.StdEncoding.EncodeToString([]byte("short")))
	if _, err := s.Open("cosign.pass", bad); err != ErrOpenFailed {
		t.Fatalf("bad transit DEK must fail closed, got %v", err)
	}
}

func TestNewTransitWrapperFailClosed(t *testing.T) {
	for _, c := range []TransitConfig{
		{KeyName: "k", TokenRef: "env:X"},                        // no base URL
		{BaseURL: "https://x", TokenRef: "env:X"},                // no key
		{BaseURL: "https://x", KeyName: "k"},                     // no token ref
	} {
		if _, err := NewTransitWrapper(c); err == nil {
			t.Fatalf("expected fail-closed for %+v", c)
		}
	}
}
