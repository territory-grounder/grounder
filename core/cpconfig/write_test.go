package cpconfig

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateWriteAcceptsConsoleWritableKey(t *testing.T) {
	k, err := ValidateWrite("session.ttl", "8h")
	if err != nil {
		t.Fatalf("writable key refused: %v", err)
	}
	if k.Name != "session.ttl" || k.Law || !k.ConsoleWritable {
		t.Fatalf("returned key is not the registry identity: %+v", k)
	}
}

func TestValidateWriteRejectsLawKey(t *testing.T) {
	// The clamp is the law: EVERY law key refuses a write, whatever the value.
	for _, k := range Registry() {
		if !k.Law {
			continue
		}
		if _, err := ValidateWrite(k.Name, "on"); !errors.Is(err, ErrLawPinned) {
			t.Errorf("law key %q: got %v, want ErrLawPinned", k.Name, err)
		}
	}
}

func TestValidateWriteRejectsUnknownAndBootOnlyKeys(t *testing.T) {
	if _, err := ValidateWrite("no.such.key", "v"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key: got %v, want ErrUnknownKey", err)
	}
	if _, err := ValidateWrite("net.public_addr", ":9999"); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("boot-only key: got %v, want ErrNotWritable", err)
	}
}

func TestValidateWriteRejectsOutOfBoundsValues(t *testing.T) {
	cases := map[string]string{
		"empty":        "",
		"oversized":    strings.Repeat("x", MaxValueLen+1),
		"control char": "a\x00b",
		"newline":      "a\nb",
		"bad utf8":     string([]byte{0xff, 0xfe}),
	}
	for name, v := range cases {
		if _, err := ValidateWrite("session.ttl", v); !errors.Is(err, ErrValueBounds) {
			t.Errorf("%s: got %v, want ErrValueBounds", name, err)
		}
	}
}
