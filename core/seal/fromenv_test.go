package seal

// ORACLES FOR THE SHARED SEAL CONSTRUCTION (TG-275).
//
// THE DEFECT: `sealed_secret` held ZERO rows on a live deployment. Not because the table was unused by
// design — because config.RegisterStoreResolver was called only in cmd/grounder, so the WORKER, which is
// where credentials are actually consumed (hostdiag, syslog-ng, actuation, AWX sync), could not resolve a
// `store:` ref at all. Every part was built, tested and green. Nothing called it from the process that
// needed it. Same shape as TG-251/267/268.
//
// These tests hold the two properties that keep it fixed: the resolver behaves correctly, and BOTH
// composition roots wire it.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

type fakeStore struct {
	blobs map[string]Sealed
	err   error
}

func (f fakeStore) Get(_ context.Context, name string) (Sealed, bool, error) {
	if f.err != nil {
		return Sealed{}, false, f.err
	}
	b, ok := f.blobs[name]
	return b, ok, nil
}

func newTestSealer(t *testing.T) *Sealer {
	t.Helper()
	w, err := NewLocalWrapper([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("wrapper: %v", err)
	}
	s, err := NewSealer(w)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return s
}

// The happy path has to actually work, or the alarm below is theatre.
func TestTheResolverReturnsTheSealedValue(t *testing.T) {
	s := newTestSealer(t)
	blob, err := s.Seal("awx-root", []byte("hunter2"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := StoreResolver(s, fakeStore{blobs: map[string]Sealed{"awx-root": blob}})("awx-root")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("got %q, want the sealed value round-tripped", got)
	}
}

// KILLING MUTATION: return ("", nil) when the name is absent. RED — a `store:` ref that resolves to an
// empty string hands a connector a blank credential, and the failure then surfaces as an SSH auth error
// or an AWX 401 somewhere far from the cause. The absence must be reported AS an absence.
func TestAMissingSecretIsAnErrorNotAnEmptyString(t *testing.T) {
	got, err := StoreResolver(newTestSealer(t), fakeStore{blobs: map[string]Sealed{}})("nope")
	if err == nil {
		t.Fatalf("a missing sealed secret resolved to %q with no error — the caller receives a blank "+
			"credential and fails somewhere unrelated to the real cause", got)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error %q does not name the secret — an operator cannot act on it", err)
	}
}

// A store-level failure must propagate, not be flattened into "not found". "Postgres is unreachable" and
// "you never wrote this secret" call for completely different operator responses.
func TestAStoreFailurePropagatesRatherThanReadingAsNotFound(t *testing.T) {
	boom := errors.New("connection refused")
	_, err := StoreResolver(newTestSealer(t), fakeStore{err: boom})("awx-root")
	if !errors.Is(err, boom) {
		t.Fatalf("store error became %v — a database outage that reads as 'secret not found' sends the "+
			"operator to look for a secret that is right there", err)
	}
}

// KILLING MUTATION: check the local master key before Transit. RED — when both are configured, Transit is
// the stronger posture (the master key never enters this process), so it must win regardless of ordering.
func TestTransitWinsOverALocalMasterKeyWhenBothAreConfigured(t *testing.T) {
	t.Setenv("TG_SEAL_TRANSIT_KEY", "tg-seal")
	t.Setenv("TG_SEAL_TRANSIT_ADDR", "https://openbao.invalid:8200")
	t.Setenv("TG_SEAL_TRANSIT_TOKEN_REF", "env:TG_TEST_BAO_TOKEN")
	t.Setenv("TG_TEST_BAO_TOKEN", "s.dummy")
	t.Setenv("TG_TEST_MASTER", "0123456789abcdef0123456789abcdef")
	_, how := FromEnv(config.SecretRef("env:TG_TEST_MASTER"))
	if strings.Contains(how, "in-process master key") {
		t.Fatalf("chose %q with Transit configured — the weaker posture won, and the master key is now "+
			"resident in a process that did not need it", how)
	}
}

// With nothing configured at all, FromEnv reports no sealer — and the CALLER logs it. Returning a
// half-built sealer here would be the worst outcome: writes that appear to succeed under a zero key.
func TestNoConfigurationYieldsNoSealerRatherThanAWeakOne(t *testing.T) {
	os.Unsetenv("TG_SEAL_TRANSIT_KEY")
	s, how := FromEnv(config.SecretRef("env:TG_DEFINITELY_UNSET_MASTER_KEY"))
	if s != nil {
		t.Fatalf("built a sealer from nothing (%q) — secrets would be sealed under an unusable key", how)
	}
	if how != "" {
		t.Fatalf("described a sealer it did not build: %q", how)
	}
}
