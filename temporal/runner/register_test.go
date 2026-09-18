package runner

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// capturingRegistry records the func name of every registration so the parity oracle can compare the
// canonical list against the type's real method set.
type capturingRegistry struct{ names map[string]bool }

func (c *capturingRegistry) RegisterActivity(a interface{}) {
	full := runtime.FuncForPC(reflect.ValueOf(a).Pointer()).Name()
	// method value names look like ".../runner.(*Activities).RecordPendingActivity-fm"
	name := strings.TrimSuffix(full[strings.LastIndex(full, ".")+1:], "-fm")
	c.names[name] = true
}

// TestRegisterActivitiesCoversEveryActivityMethod is the registration-parity guard: every exported
// method on *Activities whose name ends in "Activity" MUST be registered by RegisterActivities. This is
// the CI tripwire for the 2026-07-18 failure class (an activity the workflow schedules that the
// production worker never registered — green tests, dark prod).
func TestRegisterActivitiesCoversEveryActivityMethod(t *testing.T) {
	reg := &capturingRegistry{names: map[string]bool{}}
	RegisterActivities(reg, &Activities{})

	typ := reflect.TypeOf(&Activities{})
	var missing []string
	total := 0
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		if !strings.HasSuffix(m.Name, "Activity") {
			continue
		}
		total++
		if !reg.names[m.Name] {
			missing = append(missing, m.Name)
		}
	}
	if total == 0 {
		t.Fatal("reflection found no *Activity methods — the guard itself is broken")
	}
	if len(missing) > 0 {
		t.Fatalf("RegisterActivities does not register %v — add them to the canonical list in register.go (every harness AND the production worker registers through it)", missing)
	}
	if extra := len(reg.names) - total; extra != 0 {
		t.Fatalf("RegisterActivities registered %d entries but *Activities has %d *Activity methods — stale entry in register.go?", len(reg.names), total)
	}
}
