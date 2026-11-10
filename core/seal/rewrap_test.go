package seal

// ORACLES FOR THE DEK REWRAP (TG-163).
//
// The property under test is the one that makes master-key rotation survivable: re-wrapping a stored DEK
// under a new key version must leave the SECRET readable and the value ciphertext byte-identical. Get that
// wrong and the failure is silent and total — the row still looks fine, and the credential is gone.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// KILLING MUTATION (executed 2026-08-04): make Sealer.RewrapDEK a silent no-op — `return in, nil`. RED
// here with "rewrap produced a byte-identical wrapped DEK — nothing was re-keyed, so a run over this store
// would report success while leaving the old key version load-bearing". That is the dangerous shape: the
// run reports success, the operator retires the old Transit key version, and every credential dies at once.
//
// A second mutation on the same line — returning only the key side and dropping in.Ciphertext/in.Nonce —
// is caught by the value-side equality check below.
func TestARewrappedEnvelopeStillOpensToTheSameSecret(t *testing.T) {
	s := localSealer(t)
	const name, secret = "tg163.local", "hunter2-the-actual-credential"
	blob, err := s.Seal(name, []byte(secret))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	next, err := s.RewrapDEK(name, blob)
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	got, err := s.Open(name, next)
	if err != nil {
		t.Fatalf("the secret did not survive its own rewrap: %v — a rewrap that cannot be opened is "+
			"permanent credential loss with no plaintext anywhere to restore from", err)
	}
	if string(got) != secret {
		t.Fatalf("rewrapped secret opened to %q, want %q", got, secret)
	}
	// The value side must be untouched, not merely re-derivable. A rewrap that re-encrypted the value
	// would be a far larger, non-resumable operation, and every row would change under any reader.
	if !bytes.Equal(next.Ciphertext, blob.Ciphertext) || !bytes.Equal(next.Nonce, blob.Nonce) {
		t.Fatalf("rewrap altered the VALUE ciphertext — it must touch the key side only, or a " +
			"half-finished run is no longer safe to resume")
	}
	if bytes.Equal(next.WrappedDEK, blob.WrappedDEK) && bytes.Equal(next.DEKNonce, blob.DEKNonce) {
		t.Fatalf("rewrap produced a byte-identical wrapped DEK — nothing was re-keyed, so a run over " +
			"this store would report success while leaving the old key version load-bearing")
	}
}

// The name binding must survive a rewrap: a rewrapped blob still refuses to open under another name.
func TestARewrappedEnvelopeKeepsItsNameBinding(t *testing.T) {
	s := localSealer(t)
	blob, err := s.Seal("tg163.a", []byte("value-a"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	next, err := s.RewrapDEK("tg163.a", blob)
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	if _, err := s.Open("tg163.b", next); err == nil {
		t.Fatalf("a rewrapped blob opened under a DIFFERENT name — re-labelling one credential as " +
			"another is exactly what the AAD binding exists to refuse")
	}
}

// A rewrap of an ALREADY-BROKEN row must report, not "fix". THE POINT: a rewrap run walks the whole store,
// and if it silently re-wrapped rows whose current DEK does not unwrap, it would overwrite the last
// evidence of a real fault while leaving the secret just as dead.
func TestARewrapRefusesARowThatDoesNotUnwrapToday(t *testing.T) {
	s := localSealer(t)
	blob, err := s.Seal("tg163.broken", []byte("value"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	blob.WrappedDEK[0] ^= 0xff // corrupt the wrap
	if _, err := s.RewrapDEK("tg163.broken", blob); err == nil {
		t.Fatalf("rewrapping a row whose DEK does not unwrap SUCCEEDED — the run would report a healthy " +
			"store and bury the fault")
	}
}

// verifyBreaker returns a wrapped DEK for a DIFFERENT key on rewrap — the exact silent-corruption class
// (a response that belongs to another blob) the verification step exists to catch.
type verifyBreaker struct {
	DEKWrapper
	other DEKWrapper
}

func (v verifyBreaker) RewrapDEK(name string, _, _ []byte) ([]byte, []byte, error) {
	bogus := bytes.Repeat([]byte{0x5a}, KeySize)
	return v.other.WrapDEK(name, bogus)
}

// KILLING MUTATION (executed 2026-08-04): delete the whole verification block (the second UnwrapDEK and the
// subtle.ConstantTimeCompare) from Sealer.RewrapDEK. RED here with "a rewrap that returned a DIFFERENT DEK
// was accepted — persisting 9f4a7e… over the stored wrap destroys this secret permanently", and RED again
// in the Transit test's decrypt-call count. In production that mutation writes a wrapped DEK which cannot
// decrypt the row's ciphertext, destroying the credential with no error at any layer.
func TestARewrapThatChangesTheDekIsRefusedNotPersisted(t *testing.T) {
	inner := localWrapperFor(t, bytes.Repeat([]byte{7}, KeySize))
	s, err := NewSealer(verifyBreaker{DEKWrapper: inner, other: inner})
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	blob, err := s.Seal("tg163.verify", []byte("value"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	next, err := s.RewrapDEK("tg163.verify", blob)
	if err == nil {
		t.Fatalf("a rewrap that returned a DIFFERENT DEK was accepted — persisting %x over the stored "+
			"wrap destroys this secret permanently, and nothing would report it until the credential "+
			"was next resolved", next.WrappedDEK)
	}
	if err != ErrRewrapVerify {
		t.Fatalf("want ErrRewrapVerify, got %v", err)
	}
}

// The Transit path must move the ciphertext with the REWRAP endpoint rather than re-encrypting a
// locally-held DEK.
//
// BE PRECISE ABOUT WHAT THIS DOES AND DOES NOT PROVE. It is tempting to call this "the DEK never crosses
// the wire", and that would be FALSE: RewrapDEK decrypts twice — once to learn the current DEK, once to
// verify the new wrap holds the same one — so the DEK does reach this process during a rewrap. What the
// /rewrap endpoint buys is that the RE-ENCRYPTION itself happens inside OpenBao under a key version this
// process never sees, and that no NEW capability is needed: the decrypt calls are the identical operation
// the worker performs on every ordinary `store:` resolution, with a token that already holds decrypt on
// this key. The alternative — skip verification to keep the DEK out of the process — trades a nil
// marginal exposure for silent, permanent credential destruction. This test pins the endpoint choice; the
// decrypt calls are asserted too, so nobody later "hardens" this by deleting the verification.
func TestTransitRewrapMovesCiphertextThroughTheRewrapEndpoint(t *testing.T) {
	f := &versionedTransit{version: 1}
	w, err := NewTransitWrapper(TransitConfig{
		BaseURL: "https://bao.example:8200", KeyName: "tg-seal",
		TokenRef: config.SecretRef("env:TG163_TOK"), HTTP: f,
	})
	if err != nil {
		t.Fatalf("wrapper: %v", err)
	}
	t.Setenv("TG163_TOK", "s.faketoken")
	s, err := NewSealer(w)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	blob, err := s.Seal("tg163.transit", []byte("transit-secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if v := KeyVersion(blob.WrappedDEK); v != 1 {
		t.Fatalf("sealed at key version %d, want 1", v)
	}
	f.version = 2 // the operator rotated the Transit key

	f.ops = nil
	next, err := s.RewrapDEK("tg163.transit", blob)
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	// Snapshot the calls the REWRAP made, before the Open below adds its own decrypt to the record.
	rewrapOps := append([]string(nil), f.ops...)
	if v := KeyVersion(next.WrappedDEK); v != 2 {
		t.Fatalf("rewrapped DEK sits at key version %d, want 2 — if the rewrap does not actually move the "+
			"ciphertext forward, the old key version can never be retired, which is the entire defect",
			v)
	}
	// The DEK must be recoverable under the new wrap, and the value must still open.
	if got, oerr := s.Open("tg163.transit", next); oerr != nil || string(got) != "transit-secret" {
		t.Fatalf("rewrapped Transit blob did not open: %q err=%v", got, oerr)
	}
	if !containsOp(rewrapOps, "rewrap") {
		t.Fatalf("the Transit rewrap never called /rewrap (calls: %v) — a decrypt+encrypt round trip "+
			"pulls the DEK plaintext onto the worker, discarding the only reason to wrap in OpenBao at all",
			rewrapOps)
	}
	if containsOp(rewrapOps, "encrypt") {
		t.Fatalf("the Transit rewrap called /encrypt (calls: %v) — that means it re-wrapped a "+
			"locally-held DEK instead of asking OpenBao to move its own ciphertext", rewrapOps)
	}
	// The verification decrypts. Asserted, not tolerated: if a later change "optimises" these away to keep
	// the DEK out of the process, the rewrap can no longer tell a correct response from one belonging to a
	// different blob, and the first bad response destroys a credential in silence.
	if got := countOp(rewrapOps, "decrypt"); got != 2 {
		t.Fatalf("the rewrap made %d decrypt calls (calls: %v), want 2 — one to read the current DEK and "+
			"one to verify the new wrap holds that same DEK. Dropping either turns silent, permanent "+
			"credential destruction back on", got, rewrapOps)
	}
}

// VACUITY FLOOR for KeyVersion: the version census is what tells an operator whether the old Transit key
// version can be retired. A parser that silently returns 0 for everything would report "local" for every
// row, the census would look uniform, and raising min_decryption_version would destroy the store.
func TestKeyVersionReadsRealTransitCiphertextsAndRefusesNonsense(t *testing.T) {
	positives := map[string]int{
		"vault:v1:Zm9vYmFy":  1,
		"vault:v2:Zm9vYmFy":  2,
		"vault:v17:Zm9vYmFy": 17,
	}
	matched := 0
	for in, want := range positives {
		if got := KeyVersion([]byte(in)); got != want {
			t.Fatalf("KeyVersion(%q) = %d, want %d", in, got, want)
		}
		matched++
	}
	if matched != len(positives) {
		t.Fatalf("the version parser matched %d of %d real Transit ciphertexts — a parser that matches "+
			"nothing reports every row as unversioned and makes the census a lie", matched, len(positives))
	}
	for _, in := range []string{"", "vault:", "vault:v:x", "vault:vx:y", "vault:v0:y", "rawbytes", "vault:v1"} {
		if got := KeyVersion([]byte(in)); got != 0 {
			t.Fatalf("KeyVersion(%q) = %d, want 0 — inventing a version for a non-Transit wrap would "+
				"report a locally-wrapped row as safely rotated", in, got)
		}
	}
}

// ---- helpers ----

func localSealer(t *testing.T) *Sealer {
	t.Helper()
	s, err := NewSealer(localWrapperFor(t, bytes.Repeat([]byte{3}, KeySize)))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return s
}

func localWrapperFor(t *testing.T, master []byte) DEKWrapper {
	t.Helper()
	w, err := NewLocalWrapper(master)
	if err != nil {
		t.Fatalf("wrapper: %v", err)
	}
	return w
}

func containsOp(ops []string, want string) bool { return countOp(ops, want) > 0 }

func countOp(ops []string, want string) int {
	n := 0
	for _, o := range ops {
		if o == want {
			n++
		}
	}
	return n
}

// versionedTransit is a minimal stand-in for OpenBao Transit: ciphertext is "vault:v<N>:<base64 plaintext>",
// decrypt accepts ANY version (exactly as Transit does above min_decryption_version), and rewrap re-emits
// at the CURRENT version. Crude, but it models the one behaviour this ticket turns on.
type versionedTransit struct {
	version int
	ops     []string
}

func (f *versionedTransit) Do(req *http.Request) (*http.Response, error) {
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	op := parts[len(parts)-2]
	f.ops = append(f.ops, op)
	var in map[string]string
	body, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(body, &in)

	out := map[string]map[string]string{"data": {}}
	switch op {
	case "encrypt":
		out["data"]["ciphertext"] = f.ct(in["plaintext"])
	case "rewrap":
		out["data"]["ciphertext"] = f.ct(vpayload(in["ciphertext"]))
	case "decrypt":
		out["data"]["plaintext"] = vpayload(in["ciphertext"])
	}
	b, _ := json.Marshal(out)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: http.Header{}}, nil
}

func (f *versionedTransit) ct(b64 string) string {
	return "vault:v" + vitoa(f.version) + ":" + base64.StdEncoding.EncodeToString([]byte(b64))
}

// payload recovers the wrapped base64 plaintext from "vault:vN:<base64 of it>".
func vpayload(ct string) string {
	i := strings.LastIndexByte(ct, ':')
	if i < 0 {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(ct[i+1:])
	if err != nil {
		return ""
	}
	return string(raw)
}

func vitoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
