// rewrap.go — re-wrapping a stored DEK under the CURRENT master-key version (TG-163).
//
// THE GAP THIS CLOSES. seal.go's package comment has claimed since day one that "rotation of the master
// key is a re-wrap of DEKs, not a re-entry of every secret". That was a design statement, not code: no
// re-wrap existed anywhere in the tree. Sealing runs through OpenBao Transit (TG_SEAL_TRANSIT_KEY),
// and rotating a Transit key bumps its version while every existing ciphertext stays decryptable under
// the OLD version. That is the convenient half. The inconvenient half is that nothing ever moves those
// ciphertexts forward, so the old key version can NEVER be retired — `bao transit key rotate` followed by
// `min_decryption_version=2` would make every sealed secret in the store permanently unopenable. Rotation
// was therefore a one-way door nobody could walk through.
//
// This is deliberately a CAPABILITY, not a schedule. There is no timer, no cron, no reminder: rotation
// happens when an operator decides it should, and this code's only job is to make that decision cheap and
// safe to act on.
//
// WHY THE VALUE CIPHERTEXT IS NEVER TOUCHED. A rewrap changes ONLY WrappedDEK/DEKNonce. The DEK itself is
// unchanged and the AES-256-GCM value ciphertext under it is unchanged, so a rewrap is a small, per-row,
// atomic edit rather than a decrypt-and-reseal of every credential. That is what makes a half-finished run
// harmless (see core/db/sealed_rewrap_test.go): every row is independently valid at whichever key version
// it currently sits at, because Transit decrypts any version at or above min_decryption_version.
package seal

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrRewrapVerify refuses to hand back a rewrapped envelope whose new WrappedDEK does not unwrap to the
// SAME DEK as the old one. See RewrapDEK for why this check is not paranoia.
var ErrRewrapVerify = errors.New("seal: rewrap verification failed — the new wrapped DEK does not " +
	"unwrap to the stored DEK; the envelope was NOT modified")

// DEKRewrapper is the OPTIONAL capability of re-encrypting an already-wrapped DEK from ciphertext to
// ciphertext, under the wrapper's current key version, without the caller supplying the DEK. OpenBao
// Transit implements it natively (POST /v1/transit/rewrap/<key>); a wrapper that does not implement it is
// served by Sealer.RewrapDEK's unwrap-then-wrap fallback.
//
// Note what this does and does NOT claim. The re-encryption happens inside the key service under a version
// this process never sees. It is NOT "the DEK never enters this process" — RewrapDEK below deliberately
// unwraps twice to verify, and the reasoning for that trade is stated there.
type DEKRewrapper interface {
	RewrapDEK(name string, wrapped, nonce []byte) (newWrapped, newNonce []byte, err error)
}

// RewrapDEK re-wraps the sealed envelope's DEK under the current master-key version and returns the
// updated envelope. Ciphertext and Nonce are copied through UNCHANGED — that is the contract the caller's
// conditional UPDATE depends on.
//
// WHY IT VERIFIES BEFORE RETURNING. The failure this guards is silent and permanent: persist a WrappedDEK
// that does not correspond to the row's Ciphertext and the secret is destroyed, with nothing noticing
// until someone next resolves that store: reference — possibly months later, during an incident, with no
// plaintext anywhere to recover from. The check costs one extra unwrap and turns "destroyed" into
// "refused". Two real classes of bug reach it: a response that came back for a DIFFERENT blob (Transit's
// rewrap carries no name binding — only the value-side AAD does, and that side is not consulted here), and
// a garbled/truncated ciphertext.
//
// The honest cost: verifying requires unwrapping, so on the Transit path the DEK does reach this process
// during a rewrap — two decrypt calls per row. That is the SAME call the worker already makes on every
// ordinary `store:` resolution, with a token that already holds decrypt on this key, so the marginal
// exposure is nil; and the comparison is on the DEK, never the secret value. Trading that for silent,
// permanent credential destruction would be a bad bargain, so it is not offered as an option.
func (s *Sealer) RewrapDEK(name string, in Sealed) (Sealed, error) {
	if name == "" {
		return Sealed{}, ErrOpenFailed
	}
	if len(in.WrappedDEK) == 0 {
		return Sealed{}, errors.New("seal: rewrap needs a wrapped DEK")
	}
	// The DEK as it stands today. If this fails the row is ALREADY unreadable, and a rewrap must not be
	// the operation that papers over that — it reports and leaves the row exactly as it found it.
	old, err := s.w.UnwrapDEK(name, in.WrappedDEK, in.DEKNonce)
	if err != nil {
		return Sealed{}, fmt.Errorf("seal: rewrap: current DEK does not unwrap: %w", ErrOpenFailed)
	}
	defer zero(old)

	var wrapped, nonce []byte
	if rw, ok := s.w.(DEKRewrapper); ok {
		// The strong path: the key service re-encrypts its own ciphertext, so the NEW key version is
		// applied inside OpenBao rather than by handing it material to encrypt.
		wrapped, nonce, err = rw.RewrapDEK(name, in.WrappedDEK, in.DEKNonce)
	} else {
		wrapped, nonce, err = s.w.WrapDEK(name, old)
	}
	if err != nil {
		return Sealed{}, fmt.Errorf("seal: rewrap: %w", err)
	}
	if len(wrapped) == 0 {
		return Sealed{}, errors.New("seal: rewrap produced an empty wrapped DEK")
	}

	check, err := s.w.UnwrapDEK(name, wrapped, nonce)
	if err != nil {
		return Sealed{}, ErrRewrapVerify
	}
	defer zero(check)
	if subtle.ConstantTimeCompare(check, old) != 1 {
		return Sealed{}, ErrRewrapVerify
	}

	// Value side copied through verbatim: a rewrap is a key-lifecycle operation, not a re-encryption of
	// the credential.
	return Sealed{Ciphertext: in.Ciphertext, Nonce: in.Nonce, WrappedDEK: wrapped, DEKNonce: nonce}, nil
}

// RewrapDEK asks Transit to re-encrypt an existing ciphertext under the key's LATEST version. The DEK
// plaintext never leaves OpenBao — this endpoint exists precisely so a caller can migrate ciphertexts
// forward without holding the material. nonce stays nil: Transit's ciphertext is self-describing
// (transit.go:91), and core/db.bytesOrEmpty is what keeps that nil storable (TG-276).
func (t *transitWrapper) RewrapDEK(_ string, wrapped, _ []byte) ([]byte, []byte, error) {
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := t.call("rewrap", map[string]string{"ciphertext": string(wrapped)}, &out); err != nil {
		return nil, nil, err
	}
	if out.Data.Ciphertext == "" {
		return nil, nil, errors.New("seal: transit rewrap returned no ciphertext")
	}
	return []byte(out.Data.Ciphertext), nil, nil
}

// KeyVersion reports which master-key version a wrapped DEK sits at, reading Transit's self-describing
// "vault:vN:…" prefix. 0 means "not a versioned ciphertext" — a locally-wrapped DEK, which carries no
// version because the in-process master key has none.
//
// This is the number that answers the only question rotation actually turns on: can the old key version be
// retired yet? An operator who rotates and then sets min_decryption_version needs to know that NO row is
// still at the old version. A rewrap run that reports "done" without reporting versions would not answer
// it, and guessing wrong destroys the store.
func KeyVersion(wrapped []byte) int {
	s := string(wrapped)
	if !strings.HasPrefix(s, "vault:v") {
		return 0
	}
	rest := s[len("vault:v"):]
	i := strings.IndexByte(rest, ':')
	if i <= 0 {
		return 0
	}
	n, err := strconv.Atoi(rest[:i])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
