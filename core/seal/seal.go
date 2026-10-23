// Package seal implements the sealed-secret envelope encryption (task #27 Phase D, spec/006 REQ-524).
//
// The signed-off design: pluggable key material — an env:/file: master key NOW, KMS/Vault later —
// wraps per-secret data-encryption keys (DEKs), and the DEK encrypts the secret value with an AEAD.
// Concretely: value ⟶ AES-256-GCM under a fresh random 32-byte DEK; DEK ⟶ AES-256-GCM under the
// 32-byte master key; BOTH seals bind the secret's NAME as associated data, so a sealed blob cannot be
// silently re-labeled as a different secret. The master key is resolved per use from a secret
// reference (TG_SEAL_KEY_REF) and discarded — it never rests in a struct, a row, or a response.
//
// What this protects: the database and every backup/dump of it hold only ciphertext (INV-13 — no
// plaintext secret in any artifact); rotation of the master key is a re-wrap of DEKs, not a re-entry
// of every secret. What it does NOT protect: a compromise of the running host that can read the
// master-key env/file — that boundary is INV-16's trusted substrate, unchanged by this package.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/territory-grounder/grounder/core/config"
)

// KeySize is the AES-256 key length for both the master key and every DEK.
const KeySize = 32

// MaxValueLen bounds sealed secret material (a credential, not a document).
const MaxValueLen = 8192

var (
	// ErrBadMaster refuses a master key that is not exactly KeySize bytes.
	ErrBadMaster = errors.New("seal: master key must be 32 bytes")
	// ErrBadInput refuses an empty name or an empty/oversized value.
	ErrBadInput = errors.New("seal: secret name and a value of 1..8192 bytes are required")
	// ErrOpenFailed is any authentication/decryption failure — wrong key, wrong name, or tampered
	// blob are deliberately indistinguishable.
	ErrOpenFailed = errors.New("seal: open failed (wrong key, wrong name, or tampered blob)")
)

// Sealed is one envelope-encrypted secret: the value ciphertext under its DEK, and the DEK wrapped
// under the master key. Safe to persist and to carry through a workflow — it holds no plaintext.
type Sealed struct {
	Ciphertext []byte // AES-256-GCM(value) under the DEK
	Nonce      []byte // nonce for Ciphertext
	WrappedDEK []byte // AES-256-GCM(DEK) under the master key
	DEKNonce   []byte // nonce for WrappedDEK
}

// aad binds a seal to the secret's identity: the same blob under another name refuses to open.
func aad(name string) []byte { return []byte("tg.sealed_secret\x00" + name) }

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal envelope-encrypts value under a fresh DEK wrapped by master. The DEK is zeroed before return.
func Seal(master []byte, name string, value []byte) (Sealed, error) {
	if len(master) != KeySize {
		return Sealed{}, ErrBadMaster
	}
	if name == "" || len(value) == 0 || len(value) > MaxValueLen {
		return Sealed{}, ErrBadInput
	}
	dek := make([]byte, KeySize)
	if _, err := rand.Read(dek); err != nil {
		return Sealed{}, fmt.Errorf("seal: dek: %w", err)
	}
	defer func() {
		for i := range dek {
			dek[i] = 0
		}
	}()
	dataGCM, err := newGCM(dek)
	if err != nil {
		return Sealed{}, fmt.Errorf("seal: %w", err)
	}
	nonce := make([]byte, dataGCM.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Sealed{}, fmt.Errorf("seal: nonce: %w", err)
	}
	ct := dataGCM.Seal(nil, nonce, value, aad(name))

	wrapGCM, err := newGCM(master)
	if err != nil {
		return Sealed{}, fmt.Errorf("seal: %w", err)
	}
	dekNonce := make([]byte, wrapGCM.NonceSize())
	if _, err := rand.Read(dekNonce); err != nil {
		return Sealed{}, fmt.Errorf("seal: dek nonce: %w", err)
	}
	wrapped := wrapGCM.Seal(nil, dekNonce, dek, aad(name))

	return Sealed{Ciphertext: ct, Nonce: nonce, WrappedDEK: wrapped, DEKNonce: dekNonce}, nil
}

// Open unwraps the DEK under master and decrypts the value. Every failure is ErrOpenFailed.
func Open(master []byte, name string, s Sealed) ([]byte, error) {
	if len(master) != KeySize || name == "" {
		return nil, ErrOpenFailed
	}
	wrapGCM, err := newGCM(master)
	if err != nil {
		return nil, ErrOpenFailed
	}
	dek, err := wrapGCM.Open(nil, s.DEKNonce, s.WrappedDEK, aad(name))
	if err != nil {
		return nil, ErrOpenFailed
	}
	defer func() {
		for i := range dek {
			dek[i] = 0
		}
	}()
	dataGCM, err := newGCM(dek)
	if err != nil {
		return nil, ErrOpenFailed
	}
	value, err := dataGCM.Open(nil, s.Nonce, s.Ciphertext, aad(name))
	if err != nil {
		return nil, ErrOpenFailed
	}
	return value, nil
}

// MasterKeyFromRef resolves the master-key MATERIAL by reference (env:/file:, never a literal) and
// derives the fixed-size key as SHA-256 over it. The material must carry at least 32 bytes; the
// resolved string is not retained by the caller's state — resolve per use, derive, discard.
func MasterKeyFromRef(ref config.SecretRef) ([]byte, error) {
	material, err := ref.Resolve()
	if err != nil {
		return nil, fmt.Errorf("seal: master key: %w", err)
	}
	if len(material) < 32 {
		return nil, errors.New("seal: master key material must be at least 32 bytes")
	}
	sum := sha256.Sum256([]byte(material))
	return sum[:], nil
}
