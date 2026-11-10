// master.go — the master-key operation as a pluggable boundary (spec/022 REQ-2201, TG-157/TG-153 Critical#2).
//
// seal.go envelope-encrypts a value under a per-secret DEK and wraps that DEK under a key. In the original
// design (seal.Seal/Open) the wrapping key is a 32-byte master key resolved into the process — so a host/worker
// compromise that reads TG_SEAL_KEY defeats the envelope (seal.go's own "what this does NOT protect"). This file
// makes the DEK-wrap a DEKWrapper interface so the master-key operation can be delegated to an EXTERNAL key
// service (OpenBao Transit, transit.go) that performs wrap/unwrap without ever exposing the key: the worker then
// holds no master key at all. The value-side crypto (AES-256-GCM under the DEK, name-bound AAD) is unchanged and
// shared with seal.go, so a Transit-sealed and a local-sealed blob differ ONLY in how WrappedDEK was produced.
package seal

import (
	"crypto/rand"
	"errors"
	"fmt"
)

// DEKWrapper performs the master-key operation: wrap and unwrap a data-encryption key. The name is bound so a
// wrapped DEK cannot be replayed under a different secret. For the local wrapper, nonce carries the AES-GCM
// nonce; for an external KMS whose ciphertext is self-describing (Transit), nonce is nil and ignored.
type DEKWrapper interface {
	WrapDEK(name string, dek []byte) (wrapped, nonce []byte, err error)
	UnwrapDEK(name string, wrapped, nonce []byte) (dek []byte, err error)
}

// localWrapper wraps the DEK with a 32-byte master key via AES-256-GCM — byte-for-byte the operation seal.Seal/
// Open perform, so the local path is unchanged. Kept for deployments that do not (yet) point at an external KMS.
type localWrapper struct{ master []byte }

// NewLocalWrapper builds the in-process master-key wrapper (the default when no external key service is set).
func NewLocalWrapper(master []byte) (DEKWrapper, error) {
	if len(master) != KeySize {
		return nil, ErrBadMaster
	}
	m := make([]byte, KeySize)
	copy(m, master)
	return localWrapper{master: m}, nil
}

func (w localWrapper) WrapDEK(name string, dek []byte) ([]byte, []byte, error) {
	g, err := newGCM(w.master)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("seal: dek nonce: %w", err)
	}
	return g.Seal(nil, nonce, dek, aad(name)), nonce, nil
}

func (w localWrapper) UnwrapDEK(name string, wrapped, nonce []byte) ([]byte, error) {
	g, err := newGCM(w.master)
	if err != nil {
		return nil, ErrOpenFailed
	}
	dek, err := g.Open(nil, nonce, wrapped, aad(name))
	if err != nil {
		return nil, ErrOpenFailed
	}
	return dek, nil
}

// Sealer envelope-encrypts values, delegating the DEK-wrap (the master-key operation) to a DEKWrapper. A Sealer
// built with a transit wrapper (NewTransitWrapper) holds NO master key material in the process.
type Sealer struct{ w DEKWrapper }

// NewSealer wraps a DEKWrapper into the value-side envelope encryptor.
func NewSealer(w DEKWrapper) (*Sealer, error) {
	if w == nil {
		return nil, errors.New("seal: nil DEK wrapper (fail closed)")
	}
	return &Sealer{w: w}, nil
}

// Seal produces the same envelope as seal.Seal, but the DEK is wrapped through the configured DEKWrapper.
func (s *Sealer) Seal(name string, value []byte) (Sealed, error) {
	if name == "" || len(value) == 0 || len(value) > MaxValueLen {
		return Sealed{}, ErrBadInput
	}
	dek := make([]byte, KeySize)
	if _, err := rand.Read(dek); err != nil {
		return Sealed{}, fmt.Errorf("seal: dek: %w", err)
	}
	defer zero(dek)
	dataGCM, err := newGCM(dek)
	if err != nil {
		return Sealed{}, fmt.Errorf("seal: %w", err)
	}
	nonce := make([]byte, dataGCM.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Sealed{}, fmt.Errorf("seal: nonce: %w", err)
	}
	ct := dataGCM.Seal(nil, nonce, value, aad(name))
	wrapped, dekNonce, err := s.w.WrapDEK(name, dek)
	if err != nil {
		return Sealed{}, fmt.Errorf("seal: wrap dek: %w", err)
	}
	return Sealed{Ciphertext: ct, Nonce: nonce, WrappedDEK: wrapped, DEKNonce: dekNonce}, nil
}

// Open unwraps the DEK through the configured DEKWrapper and decrypts the value. Every failure is ErrOpenFailed.
func (s *Sealer) Open(name string, sealed Sealed) ([]byte, error) {
	if name == "" {
		return nil, ErrOpenFailed
	}
	dek, err := s.w.UnwrapDEK(name, sealed.WrappedDEK, sealed.DEKNonce)
	if err != nil {
		return nil, ErrOpenFailed
	}
	defer zero(dek)
	dataGCM, err := newGCM(dek)
	if err != nil {
		return nil, ErrOpenFailed
	}
	value, err := dataGCM.Open(nil, sealed.Nonce, sealed.Ciphertext, aad(name))
	if err != nil {
		return nil, ErrOpenFailed
	}
	return value, nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
