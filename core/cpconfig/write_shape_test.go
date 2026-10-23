package cpconfig

// ORACLES FOR WRITE-TIME SHAPE VALIDATION (TG-262) AND CLEAR LEGALITY (TG-261).
//
// TG-260 made the WORKER enforce a field's declared Pattern/MaxLen/MaxItems at resolve time — which left
// the console returning 200 for a value the worker would later refuse, so the operator saw a saved setting
// that was not in effect and only the boot log said so. These pin the refusal where the operator is.

import (
	"errors"
	"strings"
	"testing"
)

func withKey(t *testing.T, k Key) {
	t.Helper()
	SetModuleKeys([]Key{k})
	t.Cleanup(func() { SetModuleKeys(nil) })
}

// KILLING MUTATION: drop the shapeFault call from ValidateWrite. RED — the console accepts a value the
// worker will refuse at boot, which is the exact asymmetry TG-262 names.
func TestAValueTheFieldForbidsIsRefusedAtWriteTime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   Key
		value string
	}{
		{"duration that does not parse", Key{Name: "module.a.b.iv", ConsoleWritable: true, Type: "duration"}, "soon"},
		{"bool that is not a bool", Key{Name: "module.a.b.on", ConsoleWritable: true, Type: "bool"}, "maybe"},
		{"url with no scheme", Key{Name: "module.a.b.url", ConsoleWritable: true, Type: "url"}, "host:8006"},
		{"over the field's MaxLen", Key{Name: "module.a.b.s", ConsoleWritable: true, Type: "text", MaxLen: 8}, "far too long to fit"},
		{"violating the field's Pattern", Key{Name: "module.a.b.n", ConsoleWritable: true, Type: "text", Pattern: `^[0-9]+$`}, "not-a-number"},
		{"more entries than MaxItems", Key{Name: "module.a.b.l", ConsoleWritable: true, Type: "idlist", MaxItems: 2}, "a,b,c"},
		{"one bad entry in a list", Key{Name: "module.a.b.l", ConsoleWritable: true, Type: "idlist", Pattern: `^[0-9]+$`}, "1,2,three"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withKey(t, tc.key)
			_, err := ValidateWrite(tc.key.Name, tc.value)
			if !errors.Is(err, ErrValueShape) {
				t.Fatalf("got %v, want ErrValueShape — the console would accept a value the worker refuses", err)
			}
		})
	}
}

// KILLING MUTATION: make shapeFault reject everything. RED — a check that refuses all values would pass
// every test above while making the dialog unusable.
func TestAWellFormedValueStillPasses(t *testing.T) {
	for _, tc := range []struct {
		key   Key
		value string
	}{
		{Key{Name: "module.a.b.iv", ConsoleWritable: true, Type: "duration"}, "30s"},
		{Key{Name: "module.a.b.on", ConsoleWritable: true, Type: "bool"}, "true"},
		{Key{Name: "module.a.b.url", ConsoleWritable: true, Type: "url"}, "https://pve01.example.test:8006"},
		{Key{Name: "module.a.b.n", ConsoleWritable: true, Type: "text", Pattern: `^[0-9]+$`}, "12345"},
		{Key{Name: "module.a.b.l", ConsoleWritable: true, Type: "idlist", MaxItems: 3, Pattern: `^[0-9]+$`}, "1,2,3"},
		// An UNCONSTRAINED key — every compiled control-plane key is one — must be unaffected.
		{Key{Name: "module.a.b.free", ConsoleWritable: true}, "anything at all"},
	} {
		withKey(t, tc.key)
		if _, err := ValidateWrite(tc.key.Name, tc.value); err != nil {
			t.Errorf("%s value %q refused: %v", tc.key.Type, tc.value, err)
		}
	}
}

// The refusal must tell the operator what IS allowed — a rejection they cannot act on sends them to the
// logs, which is where TG-262's defect already sent them.
//
// KILLING MUTATION: drop helpSuffix. RED.
func TestTheRefusalCarriesTheFieldsOwnHelp(t *testing.T) {
	k := Key{Name: "module.a.b.iv", ConsoleWritable: true, Type: "duration",
		Help: "How long between polls, e.g. 30s."}
	withKey(t, k)
	_, err := ValidateWrite(k.Name, "soon")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "How long between polls") {
		t.Fatalf("refusal %q does not carry the field's own help — the operator is told they are wrong "+
			"without being told what is right", err)
	}
}

// KILLING MUTATION: let ValidateClear skip the LAW / console-writable checks. RED — clearing must obey
// exactly the rules writing does, or "un-set" becomes a way to reach a key writing cannot.
func TestClearObeysTheSameRegistryRulesAsAWrite(t *testing.T) {
	if _, err := ValidateClear("no.such.key"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("unknown key: got %v, want ErrUnknownKey", err)
	}
	// A LAW key: pinned against writes AND clears.
	if _, err := ValidateClear("safety.may_actuate"); !errors.Is(err, ErrLawPinned) {
		t.Errorf("law key: got %v, want ErrLawPinned", err)
	}
	// A registered but boot-only key.
	if _, err := ValidateClear("net.public_addr"); !errors.Is(err, ErrNotWritable) {
		t.Errorf("boot-only key: got %v, want ErrNotWritable", err)
	}
	// The control: a legitimately writable key CAN be cleared, so the refusals above discriminate.
	if _, err := ValidateClear("session.ttl"); err != nil {
		t.Errorf("a console-writable key could not be cleared: %v", err)
	}
}
