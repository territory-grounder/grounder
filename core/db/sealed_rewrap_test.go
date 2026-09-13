package db

// ORACLE FOR THE INTERRUPTIBLE DEK REWRAP (TG-163).
//
// THE DEFECT THIS LANE FIXES. Sealing runs through OpenBao Transit. `bao transit key rotate` bumps the key
// version and leaves existing ciphertexts readable under the OLD version — so rotation appears to work and
// nothing breaks. But nothing ever moved stored DEKs forward, so the old version stayed load-bearing
// forever: setting min_decryption_version to retire it would have made every sealed credential in the
// estate permanently unopenable. seal.go has claimed since day one that "rotation of the master key is a
// re-wrap of DEKs"; until TG-163 no re-wrap existed anywhere in the tree.
//
// WHY THIS RUNS AGAINST A REAL POSTGRES. The property the ticket asks for is about DURABLE state across an
// interruption: a store that is half at the old key version and half at the new must be FULLY readable, and
// a row re-put underneath a running rewrap must not be clobbered by it. Both are properties of rows and of
// the UPDATE's WHERE clause. A fake store would assert the test author's model of the SQL, which is exactly
// the thing under test — the same trap TG-276 fell into, where local and Transit halves were each correct
// and had never been run against each other.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/seal"
)

// versionedWrapper models the ONE OpenBao Transit behaviour this ticket turns on: ciphertexts are
// self-describing and tagged with the key version that produced them, ANY version still decrypts (that is
// what makes a half-rewrapped store readable), and rewrap moves a ciphertext to the current version without
// the caller supplying the DEK. Implemented over real AES-GCM so a mis-keyed unwrap genuinely fails rather
// than being waved through by a stub.
type versionedWrapper struct {
	keys    map[int][]byte // version -> 32-byte wrapping key
	current int
}

func newVersionedWrapper() *versionedWrapper {
	w := &versionedWrapper{keys: map[int][]byte{}, current: 1}
	w.keys[1] = bytes.Repeat([]byte{0x11}, seal.KeySize)
	return w
}

// rotate is `bao transit key rotate`: a new version, and every OLD version still decrypts.
func (w *versionedWrapper) rotate() {
	w.current++
	w.keys[w.current] = bytes.Repeat([]byte{byte(0x10 + w.current)}, seal.KeySize)
}

func (w *versionedWrapper) gcm(version int) (cipher.AEAD, error) {
	k, ok := w.keys[version]
	if !ok {
		return nil, fmt.Errorf("no key version %d", version)
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func (w *versionedWrapper) WrapDEK(_ string, dek []byte) ([]byte, []byte, error) {
	g, err := w.gcm(w.current)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	ct := g.Seal(nonce, nonce, dek, nil)
	// The self-describing shape seal.KeyVersion parses, so the version census is exercised too.
	return []byte("vault:v" + strconv.Itoa(w.current) + ":" + base64.StdEncoding.EncodeToString(ct)), nil, nil
}

func (w *versionedWrapper) UnwrapDEK(_ string, wrapped, _ []byte) ([]byte, error) {
	v := seal.KeyVersion(wrapped)
	if v == 0 {
		return nil, seal.ErrOpenFailed
	}
	g, err := w.gcm(v)
	if err != nil {
		return nil, seal.ErrOpenFailed
	}
	s := string(wrapped)
	raw, err := base64.StdEncoding.DecodeString(s[strings.LastIndexByte(s, ':')+1:])
	if err != nil || len(raw) < g.NonceSize() {
		return nil, seal.ErrOpenFailed
	}
	dek, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], nil)
	if err != nil {
		return nil, seal.ErrOpenFailed
	}
	return dek, nil
}

// RewrapDEK satisfies seal.DEKRewrapper, so the Sealer takes the ciphertext-to-ciphertext path.
func (w *versionedWrapper) RewrapDEK(name string, wrapped, nonce []byte) ([]byte, []byte, error) {
	dek, err := w.UnwrapDEK(name, wrapped, nonce)
	if err != nil {
		return nil, nil, err
	}
	return w.WrapDEK(name, dek)
}

func sealerOver(t *testing.T, w seal.DEKWrapper) *seal.Sealer {
	t.Helper()
	s, err := seal.NewSealer(w)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return s
}

// THE TICKET'S CENTRAL PROPERTY: a rewrap interrupted half-way leaves a store that is FULLY readable.
//
// KILLING MUTATION (executed 2026-08-04): make seal.Sealer.RewrapDEK return its input unchanged —
// `return in, nil` — so the rewrap reports success while re-keying nothing. RED here at the vacuity floor:
//
//	the store is not actually half-rewrapped (v1=6 v2=0, want v1=3 v2=3)
//
// That is the mutation worth guarding against, because it is the one that LOOKS fine. A rewrap that walks
// the store, reports "6 rewrapped", and moves no ciphertext leads an operator straight to
// `min_decryption_version=2`, which destroys every credential at once. The same mutation is RED in
// core/seal/rewrap_test.go from two other directions.
//
// A second mutation was tried and REJECTED as worthless: dropping `dek_nonce = $5` from the SET clause
// fails with "mismatched param and argument count" — a compile-shaped error, not evidence about behaviour.
// Recorded so nobody re-derives it and mistakes it for an oracle.
func TestAHalfRewrappedStoreIsStillFullyReadable(t *testing.T) {
	ctx := context.Background()
	p, err := Connect(ctx, skipWithoutSealDB(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	store := NewSealedSecretStore(p)

	w := newVersionedWrapper()
	sealer := sealerOver(t, w)

	// Six credentials sealed under key version 1, exactly as a live store would hold them.
	names := []string{"tg163a", "tg163b", "tg163c", "tg163d", "tg163e", "tg163f"}
	want := map[string]string{}
	defer func() {
		for _, n := range names {
			_, _ = p.Exec(ctx, `DELETE FROM sealed_secret WHERE name = $1`, n)
		}
	}()
	for i, n := range names {
		_, _ = p.Exec(ctx, `DELETE FROM sealed_secret WHERE name = $1`, n)
		val := fmt.Sprintf("credential-value-%d-for-%s", i, n)
		want[n] = val
		blob, serr := sealer.Seal(n, []byte(val))
		if serr != nil {
			t.Fatalf("seal %s: %v", n, serr)
		}
		if perr := store.Put(ctx, n, blob, "tg163 oracle", "test", int64(i+1), 1); perr != nil {
			t.Fatalf("put %s: %v", n, perr)
		}
	}

	w.rotate() // the operator rotates the Transit key: current is now v2

	// Rewrap only the FIRST THREE, then stop dead — the worker was killed, the pod was evicted, the
	// operator hit the limit. This is the state the ticket says must be safe.
	rows, err := store.ListWrappedDEKs(ctx, "")
	if err != nil {
		t.Fatalf("list wrapped: %v", err)
	}
	scoped := scopeToNames(rows, names)
	if len(scoped) != len(names) {
		t.Fatalf("the rewrap walk found %d of %d rows it must re-key — a walk that cannot see the store "+
			"would report a clean rotation having touched nothing", len(scoped), len(names))
	}
	const stopAfter = 3
	var cursor string
	for i, row := range scoped {
		if i >= stopAfter {
			break
		}
		next, rerr := sealer.RewrapDEK(row.Name, seal.Sealed{WrappedDEK: row.WrappedDEK, DEKNonce: row.DEKNonce})
		if rerr != nil {
			t.Fatalf("rewrap %s: %v", row.Name, rerr)
		}
		landed, uerr := store.RewrapDEK(ctx, row.Name, row.WrappedDEK, row.DEKNonce, next.WrappedDEK, next.DEKNonce)
		if uerr != nil || !landed {
			t.Fatalf("rewrap store %s: landed=%v err=%v", row.Name, landed, uerr)
		}
		cursor = row.Name
	}

	// VACUITY FLOOR. If the store were NOT genuinely mixed, the readability assertion below would pass for
	// a reason that has nothing to do with the property under test — a rewrap that silently did nothing,
	// or one that somehow did everything, would both sail through it.
	mixed, err := store.ListWrappedDEKs(ctx, "")
	if err != nil {
		t.Fatalf("list wrapped: %v", err)
	}
	census := map[int]int{}
	for _, r := range scopeToNames(mixed, names) {
		census[seal.KeyVersion(r.WrappedDEK)]++
	}
	if census[1] != len(names)-stopAfter || census[2] != stopAfter {
		t.Fatalf("the store is not actually half-rewrapped (v1=%d v2=%d, want v1=%d v2=%d) — this test "+
			"then proves nothing about surviving an interruption",
			census[1], census[2], len(names)-stopAfter, stopAfter)
	}

	// THE ASSERTION. Every row — old version and new — must still open to its original value.
	for _, n := range names {
		blob, found, gerr := store.Get(ctx, n)
		if gerr != nil || !found {
			t.Fatalf("%s vanished from the store (found=%v err=%v)", n, found, gerr)
		}
		got, oerr := sealer.Open(n, blob)
		if oerr != nil {
			t.Fatalf("%s could NOT be opened after a half-finished rewrap: %v\n\nThis is the failure that "+
				"makes rotation unusable: an interrupted re-key would leave part of the estate's "+
				"credentials permanently unrecoverable, and nothing would report it until each one was "+
				"next resolved.", n, oerr)
		}
		if string(got) != want[n] {
			t.Fatalf("%s opened to %q, want %q — a rewrap must never change the VALUE", n, got, want[n])
		}
	}

	// AND IT RESUMES. Picking up at the cursor finishes the job and leaves ONE key version in use, which is
	// the only state in which the old version may be retired.
	rest, err := store.ListWrappedDEKs(ctx, cursor)
	if err != nil {
		t.Fatalf("list wrapped after cursor: %v", err)
	}
	restScoped := scopeToNames(rest, names)
	if len(restScoped) != len(names)-stopAfter {
		t.Fatalf("resuming after %q found %d rows, want %d — a cursor that does not resume where the run "+
			"stopped either re-does work or, worse, skips rows and leaves them on the old key version",
			cursor, len(restScoped), len(names)-stopAfter)
	}
	for _, row := range restScoped {
		next, rerr := sealer.RewrapDEK(row.Name, seal.Sealed{WrappedDEK: row.WrappedDEK, DEKNonce: row.DEKNonce})
		if rerr != nil {
			t.Fatalf("resume rewrap %s: %v", row.Name, rerr)
		}
		if landed, uerr := store.RewrapDEK(ctx, row.Name, row.WrappedDEK, row.DEKNonce, next.WrappedDEK, next.DEKNonce); uerr != nil || !landed {
			t.Fatalf("resume rewrap store %s: landed=%v err=%v", row.Name, landed, uerr)
		}
	}
	final, err := store.ListWrappedDEKs(ctx, "")
	if err != nil {
		t.Fatalf("final list: %v", err)
	}
	for _, r := range scopeToNames(final, names) {
		if v := seal.KeyVersion(r.WrappedDEK); v != 2 {
			t.Fatalf("%s is still on key version %d after a completed rewrap — the old version can never "+
				"be retired, which is the whole defect TG-163 exists to close", r.Name, v)
		}
	}
	for _, n := range names {
		blob, _, _ := store.Get(ctx, n)
		got, oerr := sealer.Open(n, blob)
		if oerr != nil || string(got) != want[n] {
			t.Fatalf("%s did not survive the COMPLETED rewrap: %q err=%v", n, got, oerr)
		}
	}
}

// THE RACE. A rewrap run reads a row, asks the key service to re-key it, and writes the result back. An
// administrator can re-put that same secret in between — PutSecretActivity replaces the ciphertext AND the
// wrapped DEK with an entirely new DEK. Stamping the run's stale wrap over that row destroys the new
// secret: the value is unrecoverable and NOTHING reports it until the credential is next resolved.
//
// KILLING MUTATION (executed 2026-08-04): make the UPDATE in SealedSecretStore.RewrapDEK unconditional —
// `UPDATE sealed_secret SET wrapped_dek = $2, dek_nonce = $3 WHERE name = $1`. RED here with:
//
//	the re-put secret was DESTROYED by the rewrap: seal: open failed (wrong key, wrong name, or tampered blob)
//
// which is the production consequence stated exactly: a credential that no longer exists, with no plaintext
// anywhere to restore from, discovered whenever it is next resolved.
func TestARewrapRefusesToClobberASecretRePutUnderneathIt(t *testing.T) {
	ctx := context.Background()
	p, err := Connect(ctx, skipWithoutSealDB(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	store := NewSealedSecretStore(p)

	w := newVersionedWrapper()
	sealer := sealerOver(t, w)
	const name = "tg163race"
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM sealed_secret WHERE name = $1`, name) }()
	_, _ = p.Exec(ctx, `DELETE FROM sealed_secret WHERE name = $1`, name)

	original, err := sealer.Seal(name, []byte("the-ORIGINAL-credential"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := store.Put(ctx, name, original, "tg163 race", "test", 1, 1); err != nil {
		t.Fatalf("put: %v", err)
	}

	// The rewrap run reads the row it is about to re-key...
	rows, err := store.ListWrappedDEKs(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var seen WrappedDEKRow
	for _, r := range rows {
		if r.Name == name {
			seen = r
		}
	}
	if seen.Name == "" {
		t.Fatalf("the row under test was not returned by the walk — the race below would be unreachable " +
			"and this test would pass vacuously")
	}
	w.rotate()
	next, err := sealer.RewrapDEK(seen.Name, seal.Sealed{WrappedDEK: seen.WrappedDEK, DEKNonce: seen.DEKNonce})
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}

	// ...and BEFORE it writes, an administrator re-puts the secret with a new value under a brand-new DEK.
	const replacement = "the-REPLACEMENT-credential"
	fresh, err := sealer.Seal(name, []byte(replacement))
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}
	if err := store.Put(ctx, name, fresh, "tg163 race", "test", 2, 1); err != nil {
		t.Fatalf("re-put: %v", err)
	}

	landed, err := store.RewrapDEK(ctx, name, seen.WrappedDEK, seen.DEKNonce, next.WrappedDEK, next.DEKNonce)
	if err != nil {
		t.Fatalf("rewrap store: %v", err)
	}
	// THE CONSEQUENCE IS ASSERTED BEFORE THE MECHANISM. `landed` is how the bug happens; a destroyed
	// credential is what the bug IS, and that is what a failure here should say out loud.
	blob, found, err := store.Get(ctx, name)
	if err != nil || !found {
		t.Fatalf("row missing after the race (found=%v err=%v)", found, err)
	}
	got, oerr := sealer.Open(name, blob)
	if oerr != nil {
		t.Fatalf("the re-put secret was DESTROYED by the rewrap: %v\n\nThe stale wrapped DEK was stamped "+
			"over a ciphertext it does not belong to. There is no plaintext anywhere to restore from, and "+
			"the loss surfaces only when the credential is next resolved.", oerr)
	}
	if string(got) != replacement {
		t.Fatalf("after the race the secret reads %q, want the re-put value %q", got, replacement)
	}
	if landed {
		t.Fatalf("the rewrap reported that it updated a row that had been re-put underneath it — it must " +
			"lose this race, because the DEK it computed belongs to a ciphertext that no longer exists. " +
			"The secret survived here only because the new DEK happened to round-trip; the report is " +
			"still wrong, and the run would count this row as re-keyed when it was not.")
	}
}

// VACUITY FLOOR for the walk itself: the cursor must genuinely exclude, and the walk must genuinely find.
// A ListWrappedDEKs that returned nothing would make every rewrap run report a serene, meaningless success
// — the report that gets an operator to retire a key version that is still holding the store up.
func TestTheRewrapWalkFindsRowsAndItsCursorExcludes(t *testing.T) {
	ctx := context.Background()
	p, err := Connect(ctx, skipWithoutSealDB(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	store := NewSealedSecretStore(p)
	sealer := sealerOver(t, newVersionedWrapper())

	names := []string{"tg163walk1", "tg163walk2", "tg163walk3"}
	defer func() {
		for _, n := range names {
			_, _ = p.Exec(ctx, `DELETE FROM sealed_secret WHERE name = $1`, n)
		}
	}()
	for i, n := range names {
		_, _ = p.Exec(ctx, `DELETE FROM sealed_secret WHERE name = $1`, n)
		blob, serr := sealer.Seal(n, []byte("v"+strconv.Itoa(i)))
		if serr != nil {
			t.Fatalf("seal: %v", serr)
		}
		if perr := store.Put(ctx, n, blob, "tg163 walk", "test", int64(i+1), 1); perr != nil {
			t.Fatalf("put: %v", perr)
		}
	}
	all, err := store.ListWrappedDEKs(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := len(scopeToNames(all, names)); got != len(names) {
		t.Fatalf("the walk returned %d of %d rows it must re-key — a walk that matches nothing makes " +
			"every rewrap run a false all-clear", got, len(names))
	}
	after, err := store.ListWrappedDEKs(ctx, names[1])
	if err != nil {
		t.Fatalf("list after cursor: %v", err)
	}
	got := scopeToNames(after, names)
	if len(got) != 1 || got[0].Name != names[2] {
		t.Fatalf("resuming after %q returned %d rows (%v), want exactly %q — a cursor that does not "+
			"exclude re-does work, and one that over-excludes silently strands rows on the old key version",
			names[1], len(got), namesOf(got), names[2])
	}
	// Every row must carry material: a walk that returned empty wrapped DEKs would let the rewrap "succeed"
	// against nothing at all.
	for _, r := range all {
		if len(r.WrappedDEK) == 0 {
			t.Fatalf("%s came back with an empty wrapped DEK — there would be nothing to re-key", r.Name)
		}
	}
}

// scopeToNames keeps only this test's rows: the shared test database carries other suites' sealed secrets,
// and counting those would make the assertions above depend on unrelated tests.
func scopeToNames(rows []WrappedDEKRow, names []string) []WrappedDEKRow {
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
	}
	var out []WrappedDEKRow
	for _, r := range rows {
		if keep[r.Name] {
			out = append(out, r)
		}
	}
	return out
}

func namesOf(rows []WrappedDEKRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}
