package wfauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// memNonces is the in-memory NonceStore the oracles drive; the pgx store is exercised by core/db's gated tests.
type memNonces struct {
	mu   sync.Mutex
	seen map[string]bool
	err  error
}

func (m *memNonces) Claim(_ context.Context, nonce, _ string, _ time.Time) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = map[string]bool{}
	}
	if m.seen[nonce] {
		return false, nil
	}
	m.seen[nonce] = true
	return true, nil
}

var testKey = bytes.Repeat([]byte{0x5a}, 32)

func rig(t *testing.T) (*Signer, *Verifier, *memNonces, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)
	s, err := NewSigner(testKey)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	s.now = func() time.Time { return now }
	n := &memNonces{}
	v, err := NewVerifier(testKey, n)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	v.now = func() time.Time { return now }
	return s, v, n, &now
}

// The positive path: what the grounder signs, the worker admits — exactly once.
func TestSignedEnvelopeVerifiesOnce(t *testing.T) {
	s, v, _, _ := rig(t)
	e, err := s.Sign("modetransition", "Semi-auto", "Shadow", "kyriakos", "resume", Bool(true))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if e.IsZero() || e.V != Version || e.KeyID != KeyID(testKey) || len(e.Nonce) != 32 || len(e.MAC) != 64 {
		t.Fatalf("envelope shape: %+v", e)
	}
	if err := v.Verify(context.Background(), "modetransition", e, "Semi-auto", "Shadow", "kyriakos", "resume", Bool(true)); err != nil {
		t.Fatalf("a freshly signed envelope must verify: %v", err)
	}
	// REPLAY: the identical envelope presented again is refused — the nonce was claimed.
	err = v.Verify(context.Background(), "modetransition", e, "Semi-auto", "Shadow", "kyriakos", "resume", Bool(true))
	if !errors.Is(err, ErrReplayed) {
		t.Fatalf("a second presentation must be ErrReplayed, got %v", err)
	}
}

// Every way an envelope can be wrong, and the class each one lands in. RED CONTROL EXECUTED: with the MAC
// comparison replaced by `true`, the tampered cases below verified — "a tampered envelope was admitted".
func TestEnvelopeRefusals(t *testing.T) {
	ctx := context.Background()
	fields := []string{"Semi-auto", "Shadow", "kyriakos", "resume", Bool(true)}
	sign := func(t *testing.T) (*Signer, *Verifier, Envelope, *time.Time) {
		s, v, _, now := rig(t)
		e, err := s.Sign("modetransition", fields...)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return s, v, e, now
	}
	t.Run("unsigned", func(t *testing.T) {
		_, v, _, _ := sign(t)
		if err := v.Verify(ctx, "modetransition", Envelope{}, fields...); !errors.Is(err, ErrUnsigned) {
			t.Fatalf("zero envelope must be ErrUnsigned, got %v", err)
		}
	})
	t.Run("nil verifier refuses everything", func(t *testing.T) {
		var v *Verifier
		_, _, e, _ := sign(t)
		if err := v.Verify(ctx, "modetransition", e, fields...); !errors.Is(err, ErrNoKey) {
			t.Fatalf("a nil verifier (no key) must refuse with ErrNoKey, got %v", err)
		}
	})
	t.Run("nil signer signs nothing", func(t *testing.T) {
		var s *Signer
		e, err := s.Sign("modetransition", fields...)
		if !errors.Is(err, ErrNoKey) || !e.IsZero() {
			t.Fatalf("a nil signer must return the zero envelope + ErrNoKey, got %+v %v", e, err)
		}
	})
	t.Run("tampered field", func(t *testing.T) {
		_, v, e, _ := sign(t)
		bad := append([]string{}, fields...)
		bad[0] = "Full-auto" // the attacker upgrades the target mode under a captured MAC
		if err := v.Verify(ctx, "modetransition", e, bad...); !errors.Is(err, ErrTampered) {
			t.Fatalf("a changed field must be ErrTampered, got %v", err)
		}
	})
	t.Run("tampered AdminAuthorized", func(t *testing.T) {
		_, v, e, _ := sign(t)
		s2, _, _, _ := rig(t)
		e2, _ := s2.Sign("modetransition", "Semi-auto", "Shadow", "intern", "x", Bool(false))
		e2.MAC = e.MAC // splice an admin MAC onto a non-admin request
		if err := v.Verify(ctx, "modetransition", e2, "Semi-auto", "Shadow", "intern", "x", Bool(true)); !errors.Is(err, ErrTampered) {
			t.Fatalf("a spliced MAC must be ErrTampered, got %v", err)
		}
	})
	t.Run("wrong kind", func(t *testing.T) {
		_, v, e, _ := sign(t)
		if err := v.Verify(ctx, "rulesetwrite", e, fields...); !errors.Is(err, ErrTampered) {
			t.Fatalf("the same fields under another workflow kind must be ErrTampered, got %v", err)
		}
	})
	t.Run("tampered MAC bytes", func(t *testing.T) {
		_, v, e, _ := sign(t)
		e.MAC = strings.Repeat("0", 64)
		if err := v.Verify(ctx, "modetransition", e, fields...); !errors.Is(err, ErrTampered) {
			t.Fatalf("a forged MAC must be ErrTampered, got %v", err)
		}
		e.MAC = "zz"
		if err := v.Verify(ctx, "modetransition", e, fields...); !errors.Is(err, ErrTampered) {
			t.Fatalf("a malformed MAC must be ErrTampered, got %v", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		_, v, e, now := sign(t)
		v.now = func() time.Time { return now.Add(DefaultTTL + DefaultSkew + time.Second) }
		if err := v.Verify(ctx, "modetransition", e, fields...); !errors.Is(err, ErrExpired) {
			t.Fatalf("past TTL+skew must be ErrExpired, got %v", err)
		}
	})
	t.Run("within skew still admits", func(t *testing.T) {
		_, v, e, now := sign(t)
		v.now = func() time.Time { return now.Add(DefaultTTL + DefaultSkew - time.Second) }
		if err := v.Verify(ctx, "modetransition", e, fields...); err != nil {
			t.Fatalf("inside TTL+skew must verify, got %v", err)
		}
	})
	t.Run("future-dated", func(t *testing.T) {
		_, v, e, now := sign(t)
		v.now = func() time.Time { return now.Add(-DefaultSkew - time.Minute) }
		if err := v.Verify(ctx, "modetransition", e, fields...); !errors.Is(err, ErrExpired) {
			t.Fatalf("an envelope from the future beyond skew must be ErrExpired, got %v", err)
		}
	})
	t.Run("stretched window is refused even with a valid MAC", func(t *testing.T) {
		// An attacker cannot stretch the window without breaking the MAC (the timestamps are signed), but the
		// verifier ALSO caps the window independently, so a signer bug can never mint a year-long envelope.
		s, v, _, _ := rig(t)
		s.ttl = 48 * time.Hour
		e, _ := s.Sign("modetransition", fields...)
		if err := v.Verify(ctx, "modetransition", e, fields...); !errors.Is(err, ErrExpired) {
			t.Fatalf("a >24h window must be refused, got %v", err)
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		_, v, _, _ := sign(t)
		other, _ := NewSigner(bytes.Repeat([]byte{0x11}, 32))
		e, _ := other.Sign("modetransition", fields...)
		if err := v.Verify(ctx, "modetransition", e, fields...); !errors.Is(err, ErrKeyMismatch) {
			t.Fatalf("another key's envelope must be ErrKeyMismatch, got %v", err)
		}
	})
	t.Run("wrong version", func(t *testing.T) {
		_, v, e, _ := sign(t)
		e.V = 2
		if err := v.Verify(ctx, "modetransition", e, fields...); !errors.Is(err, ErrVersion) {
			t.Fatalf("another version must be ErrVersion, got %v", err)
		}
	})
	t.Run("nonce store down refuses", func(t *testing.T) {
		s, _, _, now := rig(t)
		n := &memNonces{err: errors.New("pg down")}
		v, _ := NewVerifier(testKey, n)
		v.now = func() time.Time { return *now }
		e, _ := s.Sign("modetransition", fields...)
		if err := v.Verify(ctx, "modetransition", e, fields...); !errors.Is(err, ErrNonceStore) {
			t.Fatalf("an unreadable nonce store must refuse (never admit a possible replay), got %v", err)
		}
	})
	t.Run("garbage never reaches the nonce store", func(t *testing.T) {
		// The durable claim is the one write a flood could abuse; it happens only AFTER the MAC verifies.
		_, v, e, _ := sign(t)
		n := v.nonces.(*memNonces)
		e.MAC = strings.Repeat("0", 64)
		_ = v.Verify(ctx, "modetransition", e, fields...)
		if len(n.seen) != 0 {
			t.Fatal("a tampered envelope claimed a nonce — a flood of garbage could fill the table")
		}
	})
}

// Canon is unambiguous: field boundaries and the kind are part of what is signed.
func TestCanonHasNoCollisions(t *testing.T) {
	cases := [][]string{
		{"ab", "c"}, {"a", "bc"}, {"ab"}, {"ab", ""}, {"", "ab"}, {"a|b"}, {"a", "b"},
	}
	seen := map[string][]string{}
	for _, c := range cases {
		k := string(Canon("k", c...))
		if prev, dup := seen[k]; dup {
			t.Fatalf("Canon collision: %v and %v render identically", prev, c)
		}
		seen[k] = c
	}
	if bytes.Equal(Canon("k1", "x"), Canon("k2", "x")) {
		t.Fatal("the kind must be part of the canonical form")
	}
}

func TestParseKeyShapes(t *testing.T) {
	if _, err := ParseKey(""); !errors.Is(err, ErrNoKey) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := ParseKey("short"); !errors.Is(err, ErrKeyTooShort) {
		t.Fatalf("short: %v", err)
	}
	hexKey := strings.Repeat("ab", 32) // `openssl rand -hex 32`
	k, err := ParseKey(hexKey)
	if err != nil || len(k) != 32 {
		t.Fatalf("hex key: len=%d err=%v", len(k), err)
	}
	raw, err := ParseKey(strings.Repeat("r", 40))
	if err != nil || len(raw) != 40 {
		t.Fatalf("raw key: len=%d err=%v", len(raw), err)
	}
	if _, err := NewSigner([]byte("tiny")); !errors.Is(err, ErrKeyTooShort) {
		t.Fatalf("signer with a tiny key: %v", err)
	}
	if _, err := NewVerifier(testKey, nil); err == nil {
		t.Fatal("a verifier without a nonce store must be refused — replay protection is not optional")
	}
}

// The ledger reason names the class, never the material.
func TestRefusalReasonNamesTheClassOnly(t *testing.T) {
	r := RefusalReason("modetransition", "kyriakos", ErrTampered)
	if r != "kind=modetransition actor=kyriakos why=tampered" {
		t.Fatalf("reason: %q", r)
	}
	for _, e := range []error{ErrUnsigned, ErrNoKey, ErrVersion, ErrKeyMismatch, ErrExpired, ErrTampered, ErrReplayed, ErrNonceStore} {
		if Class(e) == "refused" || Class(e) == "ok" {
			t.Fatalf("%v has no class of its own", e)
		}
	}
	if Class(errors.New("other")) != "refused" || Class(nil) != "ok" {
		t.Fatal("Class fallbacks")
	}
}

// The actor on a REFUSED start is attacker-controlled: the ledger row it reaches is bounded and single-line.
func TestRefusalReasonBoundsAndCleansTheClaimedActor(t *testing.T) {
	huge := strings.Repeat("A", 10_000)
	r := RefusalReason("modetransition", huge, ErrUnsigned)
	if len(r) > 200 || !strings.HasSuffix(r, " why=unsigned") || !strings.Contains(r, "actor="+strings.Repeat("A", 64)+"…") {
		t.Fatalf("a 10k actor must be truncated to 64 runes, got %d bytes: %.120q", len(r), r)
	}
	r = RefusalReason("modetransition", "  op\nerator\x00:x\tmallory  ", ErrTampered)
	if strings.ContainsAny(r, "\n\x00\t") || r != "kind=modetransition actor=op?erator?:x?mallory why=tampered" {
		t.Fatalf("control characters must not reach the ledger: %q", r)
	}
	if SanitizeActor("operator:kyriakos") != "operator:kyriakos" {
		t.Fatal("a clean actor passes through unchanged")
	}
}

const (
	testSeed = "ed25519-seed:" + "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
)

func asymmetricPair(t *testing.T) (*Signer, *Verifier, string) {
	t.Helper()
	sg, err := NewSigner([]byte(testSeed))
	if err != nil {
		t.Fatal(err)
	}
	pub := "ed25519-pub:" + hex.EncodeToString(sg.priv.Public().(ed25519.PublicKey))
	v, err := NewVerifier([]byte(pub), &memNonces{})
	if err != nil {
		t.Fatal(err)
	}
	return sg, v, pub
}

// KILLING MUTATIONS: skipping ed25519.Verify - RED on "a tampered field" and "a MAC made with the public key"; removing the
// ErrWrongKeyKind check on either constructor - RED on the two wrong-half cases; reusing the HMAC path when v.pub is set -
// RED on "a valid signature".
func TestAsymmetricEnvelope(t *testing.T) {
	sg, v, pub := asymmetricPair(t)
	ctx := context.Background()
	t.Run("a valid signature", func(t *testing.T) {
		e, err := sg.Sign("vote", "a", "b")
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Verify(ctx, "vote", e, "a", "b"); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})
	t.Run("a tampered field", func(t *testing.T) {
		e, _ := sg.Sign("vote", "a", "b")
		if err := v.Verify(ctx, "vote", e, "a", "CHANGED"); !errors.Is(err, ErrTampered) {
			t.Fatalf("got %v, want ErrTampered", err)
		}
	})
	t.Run("a replayed envelope", func(t *testing.T) {
		e, _ := sg.Sign("vote", "a")
		if err := v.Verify(ctx, "vote", e, "a"); err != nil {
			t.Fatal(err)
		}
		if err := v.Verify(ctx, "vote", e, "a"); !errors.Is(err, ErrReplayed) {
			t.Fatalf("got %v, want ErrReplayed", err)
		}
	})
	t.Run("a MAC made with the public key", func(t *testing.T) {
		e, _ := sg.Sign("vote", "a")
		e.Nonce = strings.Repeat("ab", 16)
		e.MAC = hex.EncodeToString(mac([]byte(pub), Canon("vote", "a"), e))
		if err := v.Verify(ctx, "vote", e, "a"); !errors.Is(err, ErrTampered) {
			t.Fatalf("a forger holding only the public key got %v, want ErrTampered", err)
		}
	})
	t.Run("the verifier refuses the signing half", func(t *testing.T) {
		if _, err := NewVerifier([]byte(testSeed), &memNonces{}); !errors.Is(err, ErrWrongKeyKind) {
			t.Fatalf("got %v, want ErrWrongKeyKind", err)
		}
	})
	t.Run("the signer refuses the verify half", func(t *testing.T) {
		if _, err := NewSigner([]byte(pub)); !errors.Is(err, ErrWrongKeyKind) {
			t.Fatalf("got %v, want ErrWrongKeyKind", err)
		}
	})
	t.Run("a hmac envelope is refused by an asymmetric verifier", func(t *testing.T) {
		hs, err := NewSigner([]byte(strings.Repeat("k", MinKeyBytes)))
		if err != nil {
			t.Fatal(err)
		}
		e, _ := hs.Sign("vote", "a")
		if err := v.Verify(ctx, "vote", e, "a"); !errors.Is(err, ErrKeyMismatch) {
			t.Fatalf("got %v, want ErrKeyMismatch", err)
		}
	})
	t.Run("ParseKey keeps the prefixed forms intact", func(t *testing.T) {
		k, err := ParseKey(testSeed)
		if err != nil || string(k) != testSeed {
			t.Fatalf("ParseKey = %q, %v", k, err)
		}
	})
}

func TestCheckSignatureDoesNotSpendTheNonce(t *testing.T) {
	key := []byte("check-signature-test-key-0123456789abcdef")
	s, err := NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(key, &countingNonces{})
	if err != nil {
		t.Fatal(err)
	}
	env, err := s.Sign("k", "a")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := v.CheckSignature("k", env, "a"); err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
	}
	if n := v.nonces.(*countingNonces).claims; n != 0 {
		t.Fatalf("CheckSignature claimed the nonce %d time(s)", n)
	}
	if err := v.CheckSignature("k", env, "b"); err == nil {
		t.Fatal("a signature over different fields must not check")
	}
	if err := v.CheckSignedContent("k", env, "b"); err == nil {
		t.Fatal("signed content over different fields must not check")
	}
	v.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	if err := v.CheckSignature("k", env, "a"); err == nil {
		t.Fatal("an expired envelope must fail the windowed check")
	}
	if err := v.CheckSignedContent("k", env, "a"); err != nil {
		t.Fatalf("an expired envelope must still pass the content check: %v", err)
	}
	v.now = time.Now
	if err := v.Verify(context.Background(), "k", env, "a"); err != nil {
		t.Fatalf("Verify after CheckSignature: %v", err)
	}
	if n := v.nonces.(*countingNonces).claims; n != 1 {
		t.Fatalf("Verify claims = %d, want 1", n)
	}
}

type countingNonces struct{ claims int }

func (c *countingNonces) Claim(context.Context, string, string, time.Time) (bool, error) {
	c.claims++
	return true, nil
}
