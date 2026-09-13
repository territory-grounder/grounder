package secretwrite

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeLanes struct {
	lane Lane
	err  error
}

func (f fakeLanes) Lane(string, string) (Lane, error) { return f.lane, f.err }

type fakeBackend struct {
	path string
	data map[string]string
	err  error
	n    int
}

func (b *fakeBackend) WriteKV(_ context.Context, p string, d map[string]string) error {
	b.n++
	b.path, b.data = p, d
	return b.err
}

const secret = "syt_supersecret_value_1234"

func okWriter(b *fakeBackend) Writer {
	return Writer{
		Lanes:   fakeLanes{lane: Lane{Surface: "notifier", SourceType: "matrix", KVPath: "secret/data/tg/lane-chosen-by-descriptor", Field: "token"}},
		Backend: b,
	}
}

// THE PATH IS SERVER-RESOLVED. A request names a MODULE; the location comes from that module's
// descriptor. A caller who could name the path could overwrite any secret the writer's credential can
// reach — and the credential necessarily can write somewhere.
func TestPathComesFromTheResolverNotTheCaller(t *testing.T) {
	b := &fakeBackend{}
	out, err := okWriter(b).Write(context.Background(), "notifier", "matrix", secret)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	// The expected path is deliberately NOT derivable from ("notifier","matrix") — otherwise a
	// mutation that built the path from the caller's arguments would be indistinguishable from reading
	// it off the resolver, and that mutation did survive an earlier version of this test.
	if b.path != "secret/data/tg/lane-chosen-by-descriptor" || b.data["token"] != secret {
		t.Fatalf("wrote %q %v — the lane must decide the destination", b.path, b.data)
	}
	if out.KVPath != "secret/data/tg/lane-chosen-by-descriptor" || out.Field != "token" {
		t.Fatalf("outcome does not name where it landed: %+v", out)
	}
}

// THE OUTCOME CANNOT CARRY THE SECRET. This is checked structurally rather than by inspection: there must
// be no field on Outcome whose value equals what was written.
//
// KILLING MUTATION: add a Value field to Outcome and populate it. RED.
func TestOutcomeNeverCarriesTheSecret(t *testing.T) {
	b := &fakeBackend{}
	out, err := okWriter(b).Write(context.Background(), "notifier", "matrix", secret)
	if err != nil {
		t.Fatal(err)
	}
	// REFLECTION, not a hand-listed set. Listing the fields explicitly is blind to a field somebody adds
	// later — which is exactly how a secret would get echoed back: not by changing these four, but by
	// appending a fifth. The mutation that added a Value field survived a hand-listed version of this
	// test.
	rv := reflect.ValueOf(out)
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		if f.Kind() != reflect.String {
			t.Fatalf("Outcome.%s is not a string — extend this check before adding non-string fields",
				rv.Type().Field(i).Name)
		}
		if strings.Contains(f.String(), secret) {
			t.Fatalf("Outcome.%s carries the secret material", rv.Type().Field(i).Name)
		}
	}
}

// AN ERROR IS THE MOST COPIED TEXT IN AN INCIDENT and must be safe to paste. No failure path may name the
// value.
//
// KILLING MUTATION: include the value in the backend-failure wrap. RED.
func TestNoErrorPathNamesTheSecret(t *testing.T) {
	cases := []struct {
		name string
		w    Writer
		val  string
	}{
		{"backend failure", Writer{
			Lanes:   fakeLanes{lane: Lane{Surface: "n", SourceType: "m", KVPath: "secret/data/tg/m", Field: "token"}},
			Backend: &fakeBackend{err: errors.New("permission denied")},
		}, secret},
		{"unknown module", Writer{
			Lanes: fakeLanes{err: ErrUnknownModule}, Backend: &fakeBackend{},
		}, secret},
		{"no lane", Writer{
			Lanes: fakeLanes{lane: Lane{Surface: "n", SourceType: "m"}}, Backend: &fakeBackend{},
		}, secret},
		{"bad value", okWriter(&fakeBackend{}), "has a\ttab"},
	}
	for _, tc := range cases {
		_, err := tc.w.Write(context.Background(), "n", "m", tc.val)
		if err == nil {
			t.Errorf("%s: expected an error", tc.name)
			continue
		}
		if strings.Contains(err.Error(), tc.val) {
			t.Errorf("%s: the error names the secret: %v", tc.name, err)
		}
	}
}

// A module with no declared secret has nothing to write, and must not silently write to an empty path.
func TestModuleWithNoSecretLaneIsRefused(t *testing.T) {
	b := &fakeBackend{}
	w := Writer{Lanes: fakeLanes{lane: Lane{Surface: "n", SourceType: "m"}}, Backend: b}
	if _, err := w.Write(context.Background(), "n", "m", secret); !errors.Is(err, ErrNoSecretLane) {
		t.Fatalf("want ErrNoSecretLane, got %v", err)
	}
	if b.n != 0 {
		t.Fatal("a module with no lane still reached the backend")
	}
}

// Bounds. A secret is opaque, so size and encoding are the only honest checks — but whitespace and control
// characters are refused because a token stored with a trailing newline fails authentication in a way that
// looks like a WRONG secret rather than a mangled one, which is a genuinely expensive misdiagnosis.
func TestValueBounds(t *testing.T) {
	b := &fakeBackend{}
	w := okWriter(b)
	for _, bad := range []string{"", "  padded  ", "trailing\n", "with\x00null", strings.Repeat("x", MaxSecretLen+1)} {
		if _, err := w.Write(context.Background(), "notifier", "matrix", bad); !errors.Is(err, ErrValueBounds) {
			t.Errorf("value %q was accepted (err=%v)", bad[:min(len(bad), 20)], err)
		}
	}
	if b.n != 0 {
		t.Fatalf("%d out-of-bounds value(s) reached the backend", b.n)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// An unwired writer must refuse rather than nil-panic on a request that carries a live credential.
func TestUnwiredWriterRefuses(t *testing.T) {
	if _, err := (Writer{}).Write(context.Background(), "n", "m", secret); err == nil {
		t.Fatal("an unwired writer accepted a secret")
	}
}
