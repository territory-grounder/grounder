// Package wfauth is the signed authorization envelope every admin-write workflow Request and every approval
// vote carries from the grounder to the worker (TG-660, deep-review F1 / pack 12).
//
// THE GAP IT CLOSES. The Temporal frontend is reachable by every service on the backplane and, until TG-660,
// carried no transport authentication. The admin-write workflows (mode transition, ruleset / native-rule /
// object-group / config / sealed-secret / manifest / op-class / skill writes, manual rollback) all trust the
// authorization fields in their INPUT — Actor, Operator, Approver, AdminAuthorized — because the grounder's
// AuthAdminSession surface derived them from an authenticated principal. Anything else that can dial the
// frontend can start the same workflow with the same fields set, and the worker cannot tell the difference. The
// same holds for VoteSignal: a signal naming the right action id with any Voter releases a POLL_PAUSE action.
//
// THE MECHANISM. The grounder (the ONLY process that authenticates humans) signs each request with an HMAC-SHA256
// over the request KIND and its canonical authorization fields, plus a random nonce and an expiry, under a key
// held in the secret substrate (TG_WORKFLOW_AUTH_KEY_REF, resolved at boot by both processes). The worker's
// first activity verifies the envelope BEFORE any effect and refuses — ledgered as `auth:refused-unsigned` — an
// absent, tampered, expired or replayed envelope. Replay protection is a durable nonce claim (workflow_auth_nonce,
// INSERT ... ON CONFLICT DO NOTHING): the first claim wins, a second presentation of the same nonce is a replay.
//
// WHAT IT IS NOT. It is not transport security: an attacker on the backplane can still read every payload
// Temporal carries. mTLS on the frontend (TG-660 part a) is that boundary. This envelope makes a FORGED or
// REPLAYED authorization unexecutable even when the transport is open, and it is the layer that survives a
// certificate leak: the key never leaves the two processes and never enters a Temporal history (only the MAC).
//
// FAIL-CLOSED THROUGHOUT. No key ⇒ the verifier refuses everything (and says so at boot); a nil signer ⇒ the
// grounder sends an empty envelope the worker refuses. There is no "unsigned but allowed" mode: a deployment
// without the key has no admin-write lane and no human-vote lane until it sets one, exactly as a deployment
// without an admin token has no admin tier.
package wfauth

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

// Version is the envelope wire version. A verifier refuses any other value, so a future canonicalization
// change cannot be replayed against an old verifier or vice versa.
const Version = 1

// DefaultTTL bounds how long a signed request may be presented after it was minted. The grounder signs at the
// authenticated surface and starts the workflow immediately; a vote is verified when the WAITING workflow's task
// runs, so a worker that is down or backlogged for a few minutes must not turn a human's vote into an expired
// one. Five minutes covers that and keeps a captured envelope useless shortly after — and it is single-use
// regardless (the nonce is spent on first verification).
const DefaultTTL = 5 * time.Minute

// DefaultSkew tolerates clock drift between the grounder and the worker (both run on one host in production,
// but the bound is stated rather than assumed).
const DefaultSkew = 30 * time.Second

// MinKeyBytes is the smallest key the signer and verifier accept. 32 random bytes is the HMAC-SHA256 output
// width; a shorter key is a misconfiguration, not a weaker setting, so it is refused outright.
const MinKeyBytes = 32

// Envelope is the signed authorization proof carried INSIDE a workflow Request / VoteSignal. Every field is
// serialisable (Temporal history); none is secret — the MAC is a one-way proof, and the key never appears.
type Envelope struct {
	// V is the envelope version.
	V int
	// KeyID is the first 8 hex chars of sha256(key): it names WHICH key signed, so a rotation is diagnosable.
	KeyID string
	// Nonce is 32 hex chars of randomness, claimed once by the verifier (replay protection).
	Nonce string
	// IssuedAt is the unix second the grounder minted the envelope.
	IssuedAt int64
	// ExpiresAt is the unix second after which the verifier refuses it.
	ExpiresAt int64
	// MAC is the hex HMAC-SHA256 over Canon(kind, fields...) ‖ nonce ‖ issued ‖ expires ‖ version.
	MAC string
}

// IsZero reports an absent envelope (a request started by something that never signed).
func (e Envelope) IsZero() bool {
	return e.V == 0 && e.KeyID == "" && e.Nonce == "" && e.MAC == ""
}

// The typed refusals. Callers ledger the class (never the key, never the MAC) and surface a non-retryable
// error — a refused authorization is a DECISION, not a transient.
var (
	ErrUnsigned    = errors.New("wfauth: request carries no authorization envelope (unsigned)")
	ErrNoKey       = errors.New("wfauth: no signing key is configured (TG_WORKFLOW_AUTH_KEY_REF) — every admin write and vote is refused until one is set")
	ErrKeyTooShort = fmt.Errorf("wfauth: signing key is shorter than %d bytes", MinKeyBytes)
	ErrVersion     = errors.New("wfauth: envelope version is not the one this verifier speaks")
	ErrKeyMismatch = errors.New("wfauth: envelope was signed by a different key (rotation in progress, or a forgery)")
	ErrExpired     = errors.New("wfauth: envelope expired (or is dated in the future beyond the allowed skew)")
	ErrTampered    = errors.New("wfauth: envelope MAC does not match the request (tampered, or signed over different fields)")
	ErrReplayed    = errors.New("wfauth: envelope nonce was already presented (replay)")
	// ErrWrongKeyKind refuses a verify-only key on the signer or a signing key on the verifier: the separation
	// exists so the worker cannot forge, so handing it the signing half must stop the boot.
	ErrWrongKeyKind = errors.New("wfauth: wrong half of an asymmetric key: the grounder takes ed25519-seed:, the worker takes ed25519-pub:")
	ErrNonceStore   = errors.New("wfauth: the nonce store is unavailable — refusing rather than admitting a possible replay")
)

// Canon renders a request KIND and its authorization-relevant FIELDS into one unambiguous byte string: each
// element is length-prefixed, so ["ab","c"] and ["a","bc"] (and ["ab"], ["ab",""]) differ, and a field that
// contains the separator of a naive join cannot collide with two fields. The kind is the first element, so the
// same fields signed for ModeTransition never verify as a RulesetWrite.
func Canon(kind string, fields ...string) []byte {
	var out []byte
	put := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		out = append(out, n[:]...)
		out = append(out, s...)
	}
	put(kind)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(fields)))
	out = append(out, n[:]...)
	for _, f := range fields {
		put(f)
	}
	return out
}

// Bool renders a boolean authorization flag (AdminAuthorized, Approve) as a canonical field.
func Bool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// Int renders an integer field (an entry id, a row id, a version id) canonically.
func Int(i int64) string { return fmt.Sprintf("%d", i) }

// KeyID names a key without revealing it.
func KeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:4])
}

// ParseKey accepts the resolved secret as either raw bytes or a hex string (the `openssl rand -hex 32` shape
// the .env.example recipe produces), and refuses anything shorter than MinKeyBytes.
func ParseKey(secret string) ([]byte, error) {
	s := strings.TrimSpace(secret)
	if s == "" {
		return nil, ErrNoKey
	}
	if strings.HasPrefix(s, ed25519SeedPrefix) || strings.HasPrefix(s, ed25519PubPrefix) {
		return []byte(s), nil
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) >= MinKeyBytes {
		return b, nil
	}
	if len(s) < MinKeyBytes {
		return nil, ErrKeyTooShort
	}
	return []byte(s), nil
}

func mac(key []byte, canon []byte, e Envelope) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(signedMessage(canon, e))
	return h.Sum(nil)
}

func signedMessage(canon []byte, e Envelope) []byte {
	out := make([]byte, 0, len(canon)+len(e.Nonce)+26)
	out = append(out, canon...)
	out = append(out, 0)
	out = append(out, e.Nonce...)
	out = append(out, 0)
	var ts [24]byte
	binary.BigEndian.PutUint64(ts[0:8], uint64(e.IssuedAt))
	binary.BigEndian.PutUint64(ts[8:16], uint64(e.ExpiresAt))
	binary.BigEndian.PutUint64(ts[16:24], uint64(e.V))
	return append(out, ts[:]...)
}

const (
	ed25519SeedPrefix = "ed25519-seed:"
	ed25519PubPrefix  = "ed25519-pub:"
)

func parseSeed(s string) (ed25519.PrivateKey, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(s, ed25519SeedPrefix))
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, fmt.Errorf("wfauth: ed25519 seed must be %d hex-encoded bytes", ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(b), nil
}

func parsePub(s string) (ed25519.PublicKey, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(s, ed25519PubPrefix))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("wfauth: ed25519 public key must be %d hex-encoded bytes", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// Signer mints envelopes. Built once at the grounder's composition root from the resolved key.
type Signer struct {
	priv  ed25519.PrivateKey
	key   []byte
	keyID string
	ttl   time.Duration
	now   func() time.Time
	rand  func([]byte) error
}

// NewSigner fails closed on a short/absent key.
func NewSigner(key []byte) (*Signer, error) {
	if len(key) == 0 {
		return nil, ErrNoKey
	}
	if strings.HasPrefix(string(key), ed25519PubPrefix) {
		return nil, ErrWrongKeyKind
	}
	if strings.HasPrefix(string(key), ed25519SeedPrefix) {
		priv, err := parseSeed(string(key))
		if err != nil {
			return nil, err
		}
		pub := priv.Public().(ed25519.PublicKey)
		return &Signer{priv: priv, keyID: KeyID(pub), ttl: DefaultTTL, now: time.Now, rand: func(b []byte) error { _, err := rand.Read(b); return err }}, nil
	}
	if len(key) < MinKeyBytes {
		return nil, ErrKeyTooShort
	}
	return &Signer{key: key, keyID: KeyID(key), ttl: DefaultTTL, now: time.Now, rand: func(b []byte) error { _, err := rand.Read(b); return err }}, nil
}

// KeyID names the key this signer uses (boot-log material, never the key).
func (s *Signer) KeyID() string { return s.keyID }

// Sign mints an envelope over Canon(kind, fields...). A nil Signer returns the zero Envelope — the worker then
// refuses as unsigned, which is the honest outcome for a grounder that has no key.
func (s *Signer) Sign(kind string, fields ...string) (Envelope, error) {
	if s == nil {
		return Envelope{}, ErrNoKey
	}
	var nb [16]byte
	if err := s.rand(nb[:]); err != nil {
		return Envelope{}, fmt.Errorf("wfauth: nonce: %w", err)
	}
	now := s.now()
	e := Envelope{
		V:         Version,
		KeyID:     s.keyID,
		Nonce:     hex.EncodeToString(nb[:]),
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(s.ttl).Unix(),
	}
	if s.priv != nil {
		e.MAC = hex.EncodeToString(ed25519.Sign(s.priv, signedMessage(Canon(kind, fields...), e)))
		return e, nil
	}
	e.MAC = hex.EncodeToString(mac(s.key, Canon(kind, fields...), e))
	return e, nil
}

// NonceStore claims a nonce durably. fresh=false means it was already claimed (a replay). An error means the
// store could not answer — the verifier then REFUSES (ErrNonceStore) rather than admit a possible replay.
type NonceStore interface {
	Claim(ctx context.Context, nonce, kind string, expiresAt time.Time) (fresh bool, err error)
}

// Verifier checks envelopes. Built once at the worker's composition root.
type Verifier struct {
	pub    ed25519.PublicKey
	key    []byte
	keyID  string
	skew   time.Duration
	now    func() time.Time
	nonces NonceStore
}

// NewVerifier fails closed on a short/absent key or a nil nonce store (replay protection is not optional).
func NewVerifier(key []byte, nonces NonceStore) (*Verifier, error) {
	if len(key) == 0 {
		return nil, ErrNoKey
	}
	if strings.HasPrefix(string(key), ed25519SeedPrefix) {
		return nil, ErrWrongKeyKind
	}
	if strings.HasPrefix(string(key), ed25519PubPrefix) {
		if nonces == nil {
			return nil, errors.New("wfauth: a nonce store is required (replay protection is not optional)")
		}
		pub, err := parsePub(string(key))
		if err != nil {
			return nil, err
		}
		return &Verifier{pub: pub, keyID: KeyID(pub), skew: DefaultSkew, now: time.Now, nonces: nonces}, nil
	}
	if len(key) < MinKeyBytes {
		return nil, ErrKeyTooShort
	}
	if nonces == nil {
		return nil, errors.New("wfauth: a nonce store is required (replay protection is not optional)")
	}
	return &Verifier{key: key, keyID: KeyID(key), skew: DefaultSkew, now: time.Now, nonces: nonces}, nil
}

// KeyID names the key this verifier checks against.
func (v *Verifier) KeyID() string { return v.keyID }

// Verify checks e against Canon(kind, fields...). Order: absence, version, key id, window, MAC (constant-time),
// THEN the nonce claim — the durable write happens only for an otherwise-valid envelope, so a flood of garbage
// cannot fill the nonce table. A nil Verifier refuses everything with ErrNoKey.
func (v *Verifier) Verify(ctx context.Context, kind string, e Envelope, fields ...string) error {
	if err := v.CheckSignature(kind, e, fields...); err != nil {
		return err
	}
	expires := time.Unix(e.ExpiresAt, 0)
	fresh, err := v.nonces.Claim(ctx, e.Nonce, kind, expires.Add(v.skew))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNonceStore, err)
	}
	if !fresh {
		return ErrReplayed
	}
	return nil
}

// SignatureChecker confirms who signed an order and what it says, without consuming its nonce or applying the validity
// window. A later step of one signed order uses it after an earlier step has claimed the nonce, possibly much later.
type SignatureChecker interface {
	CheckSignedContent(kind string, e Envelope, fields ...string) error
}

// CheckSignedContent verifies version, key and signature only.
func (v *Verifier) CheckSignedContent(kind string, e Envelope, fields ...string) error {
	return v.check(kind, e, false, fields...)
}

// CheckSignature verifies version, key, validity window and signature, and does not touch the replay store.
func (v *Verifier) CheckSignature(kind string, e Envelope, fields ...string) error {
	return v.check(kind, e, true, fields...)
}

func (v *Verifier) check(kind string, e Envelope, window bool, fields ...string) error {
	if v == nil {
		return ErrNoKey
	}
	if e.IsZero() {
		return ErrUnsigned
	}
	if e.V != Version {
		return ErrVersion
	}
	if e.KeyID != v.keyID {
		return ErrKeyMismatch
	}
	if window {
		now := v.now()
		issued, expires := time.Unix(e.IssuedAt, 0), time.Unix(e.ExpiresAt, 0)
		if !expires.After(issued) || expires.Sub(issued) > 24*time.Hour {
			return ErrExpired
		}
		if now.After(expires.Add(v.skew)) || issued.After(now.Add(v.skew)) {
			return ErrExpired
		}
	}
	want, err := hex.DecodeString(e.MAC)
	if err != nil {
		return ErrTampered
	}
	if v.pub != nil {
		if len(want) != ed25519.SignatureSize || !ed25519.Verify(v.pub, signedMessage(Canon(kind, fields...), e), want) {
			return ErrTampered
		}
	} else {
		if len(want) != sha256.Size || !hmac.Equal(want, mac(v.key, Canon(kind, fields...), e)) {
			return ErrTampered
		}
	}
	if len(e.Nonce) < 16 {
		return ErrTampered
	}
	return nil
}

// Class names a refusal for the ledger reason: the typed class only, never the MAC or key material.
func Class(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrUnsigned):
		return "unsigned"
	case errors.Is(err, ErrNoKey):
		return "no-key"
	case errors.Is(err, ErrVersion):
		return "version"
	case errors.Is(err, ErrKeyMismatch):
		return "key-mismatch"
	case errors.Is(err, ErrExpired):
		return "expired"
	case errors.Is(err, ErrTampered):
		return "tampered"
	case errors.Is(err, ErrReplayed):
		return "replayed"
	case errors.Is(err, ErrNonceStore):
		return "nonce-store-unavailable"
	}
	return "refused"
}

// RefusalDecision is the governance-ledger decision string every verifying activity appends on a refusal.
const RefusalDecision = "auth:refused-unsigned"

// RefusalReason renders the ledger reason: the workflow kind, the claimed actor and the refusal CLASS. Never
// the envelope's MAC, nonce or key id — those would let a reader of the ledger reconstruct what to forge next.
// The actor is ATTACKER-CONTROLLED on a refused start, so it is bounded (64 runes) and stripped of control
// characters before it reaches the chain: a forged start must not be able to grow a ledger row without limit or
// break its line structure.
func RefusalReason(kind, actor string, err error) string {
	return "kind=" + kind + " actor=" + SanitizeActor(actor) + " why=" + Class(err)
}

// SanitizeActor bounds and cleans a claimed actor for the ledger (see RefusalReason).
func SanitizeActor(actor string) string {
	a := strings.TrimSpace(actor)
	a = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, a)
	if n := len([]rune(a)); n > 64 {
		a = string([]rune(a)[:64]) + "…"
	}
	return a
}

// Bytes renders a byte payload (a ruleset document, sealed ciphertext) as a canonical field: its hex SHA-256,
// so the envelope stays small and any change to the payload still breaks the MAC.
func Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// VoteKind is the envelope kind for an approval vote; VoteFields its canonical fields. The workflow's OWN
// external_ref is part of what is signed, so a vote signed for one waiting session can never release another.
const VoteKind = "vote"

// VoteFields renders the vote's authorization-relevant fields.
func VoteFields(externalRef, actionID string, approve bool, voter string) []string {
	return []string{externalRef, actionID, Bool(approve), voter}
}

// RefusedMarker is the stable prefix every Refusal error message carries, so a grounder-side unwrap of a
// Temporal ApplicationError (message only) can recognise an authorization refusal without a typed error.
const RefusedMarker = "wfauth: refused"

// Refusal is the error a verifying activity returns. It is a DECISION (never retried) and its message names
// the kind and class only.
type Refusal struct {
	Kind  string
	Class string
	Err   error
}

func (r *Refusal) Error() string {
	return RefusedMarker + " " + r.Kind + " (" + r.Class + "): " + r.Err.Error()
}
func (r *Refusal) Unwrap() error { return r.Err }

// Require is the one call every admin-write activity makes FIRST: verify the envelope on req against the
// canonical fields, and on refusal (a) append the `auth:refused-unsigned` row through ledger (best-effort —
// the refusal stands whether or not the row lands, and a ledger blip must not turn a refusal into an admission)
// and (b) return a *Refusal. A nil verifier refuses everything (ErrNoKey) — the worker has no key, so nothing
// signed can be trusted. ledger may be nil (an oracle without a chain).
func Require(ctx context.Context, v EnvelopeVerifier, kind string, e Envelope, actor string,
	ledger func(context.Context, audit.GovDecision) error, fields ...string) error {
	var err error
	if v == nil {
		err = ErrNoKey
	} else {
		err = v.Verify(ctx, kind, e, fields...)
	}
	if err == nil {
		return nil
	}
	if ledger != nil {
		_ = ledger(ctx, audit.GovDecision{
			Decision: RefusalDecision,
			Reason:   RefusalReason(kind, actor, err),
			ActionID: "wf:" + kind,
			Withheld: true,
		})
	}
	return &Refusal{Kind: kind, Class: Class(err), Err: err}
}

// EnvelopeVerifier is the narrow seam the workflow activities depend on, so an oracle can drive them with an
// in-memory verifier and the composition root with the real one.
type EnvelopeVerifier interface {
	Verify(ctx context.Context, kind string, e Envelope, fields ...string) error
}

// EnvelopeSigner is the grounder-side seam.
type EnvelopeSigner interface {
	Sign(kind string, fields ...string) (Envelope, error)
}

// Compile-time proofs that the concrete types satisfy the seams (a nil *Signer / *Verifier is a valid,
// refusing implementation — see Sign / Verify).
var (
	_ EnvelopeSigner   = (*Signer)(nil)
	_ EnvelopeVerifier = (*Verifier)(nil)
)
