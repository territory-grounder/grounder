package seal

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// testKey generates key material in-test — no literal secrets in the repo.
func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	master := testKey(t)
	value := []byte("a-credential-value-generated-for-this-test-only")
	s, err := Seal(master, "librenms.token", value)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(s.Ciphertext, value) || bytes.Contains(s.WrappedDEK, value) {
		t.Fatalf("plaintext leaked into the sealed blob")
	}
	got, err := Open(master, "librenms.token", s)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Fatalf("round trip mismatch: got %q", got)
	}
}

func TestOpenFailsClosedOnWrongKeyNameOrTamper(t *testing.T) {
	master := testKey(t)
	s, err := Seal(master, "db.password", []byte("value-for-this-test"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := Open(testKey(t), "db.password", s); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("wrong master key: got %v, want ErrOpenFailed", err)
	}
	if _, err := Open(master, "other.name", s); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("wrong name (AAD): got %v, want ErrOpenFailed", err)
	}
	tampered := s
	tampered.Ciphertext = append([]byte{}, s.Ciphertext...)
	tampered.Ciphertext[0] ^= 0x01
	if _, err := Open(master, "db.password", tampered); !errors.Is(err, ErrOpenFailed) {
		t.Fatalf("tampered ciphertext: got %v, want ErrOpenFailed", err)
	}
}

func TestSealInputBounds(t *testing.T) {
	master := testKey(t)
	if _, err := Seal(master[:16], "n", []byte("v")); !errors.Is(err, ErrBadMaster) {
		t.Fatalf("short master: got %v, want ErrBadMaster", err)
	}
	if _, err := Seal(master, "", []byte("v")); !errors.Is(err, ErrBadInput) {
		t.Fatalf("empty name: got %v, want ErrBadInput", err)
	}
	if _, err := Seal(master, "n", nil); !errors.Is(err, ErrBadInput) {
		t.Fatalf("empty value: got %v, want ErrBadInput", err)
	}
	if _, err := Seal(master, "n", make([]byte, MaxValueLen+1)); !errors.Is(err, ErrBadInput) {
		t.Fatalf("oversized value: got %v, want ErrBadInput", err)
	}
}

func TestMasterKeyFromRefDerivesAndFailsClosed(t *testing.T) {
	// file: reference with ≥32 bytes of in-test material derives a fixed-size key.
	dir := t.TempDir()
	p := filepath.Join(dir, "seal.key")
	material := make([]byte, 48)
	if _, err := rand.Read(material); err != nil {
		t.Fatalf("rand: %v", err)
	}
	// hex-expand so the file is text-safe; still ≥32 bytes of material.
	if err := os.WriteFile(p, []byte(string(rune('a'))+string(material)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	key, err := MasterKeyFromRef(config.SecretRef("file:" + p))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if len(key) != KeySize {
		t.Fatalf("derived key length %d, want %d", len(key), KeySize)
	}
	// An unresolvable reference fails closed.
	if _, err := MasterKeyFromRef(config.SecretRef("env:TG_TEST_SEAL_KEY_THAT_IS_NOT_SET")); err == nil {
		t.Fatalf("unresolvable ref did not fail")
	}
	// Short material fails closed rather than weakening the key.
	short := filepath.Join(dir, "short.key")
	if err := os.WriteFile(short, []byte("too-short"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := MasterKeyFromRef(config.SecretRef("file:" + short)); err == nil {
		t.Fatalf("short material did not fail")
	}
}
