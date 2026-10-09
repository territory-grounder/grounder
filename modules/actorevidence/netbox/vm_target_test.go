package netbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ★ EVERY HOST TG TRIAGES IS A VIRTUAL MACHINE, AND THIS READER ONLY LOOKED AT DEVICES.
//
// MEASURED LIVE 2026-07-29 against the estate's NetBox:
//   - dcim.device                     99 objects — switches, APs, cameras, routers, IoT. PHYSICAL kit.
//   - virtualization.virtualmachine  132 objects — every guest TG triages, including the whole fault pool
//
// So `resolveDevice` matched nothing and the reader failed closed on every read:
// `netbox: device "dc1ghostfolio01" not found (fail closed)` — 13 of 13 attempts, zero evidence rows
// all-time. The standing summary called this reader "dark"; it was pointed at the wrong object class.
func TestAVirtualMachineTargetResolves(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{
		// The device list answers, and answers EMPTY — exactly as the live estate does for a guest.
		"/api/dcim/devices/":                    `{"results":[]}`,
		"/api/virtualization/virtual-machines/": `{"results":[{"id":125,"name":"dc1ghostfolio01"}]}`,
		"/api/core/object-changes/": `{"results":[{"id":9,"time":"2026-07-29T10:00:00Z","user_name":"kp",` +
			`"action":{"value":"update"}}],"next":null}`,
	}}
	got, err := newReader(t, f).Read(context.Background(), "dc1ghostfolio01",
		time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC), time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("a virtual-machine target still fails to resolve: %v", err)
	}
	if len(got) != 1 || got[0].Actor != "kp" {
		t.Fatalf("expected one evidence row attributed to kp, got %+v", got)
	}
	if got[0].Target != "dc1ghostfolio01" || got[0].Domain != "netbox" {
		t.Errorf("evidence is mis-shaped: %+v", got[0])
	}
}

// ★ THE CHANGELOG QUERY MUST CARRY THE OBJECT TYPE. A NetBox change row is keyed by
// (changed_object_type, changed_object_id) — the id ALONE is not an identity, because every object class has
// its own id space.
//
// Measured 2026-07-29: of 6,863 change rows on this estate, the overwhelming majority belong to
// `slurpit_netbox.slurpitimporteddevice` / `slurpitstageddevice`, a sync plugin churning thousands of rows
// nightly at 02:00 in its own id space. An id-only filter would hand those to whichever host happened to
// share the number — a stranger's changes attributed to this incident.
//
// The bug was MASKED by the lookup bug: changes() was never reached with a real id. Fixing only the lookup
// would have UN-masked it.
func TestTheChangelogQueryIsScopedToTheResolvedObjectType(t *testing.T) {
	for _, c := range []struct {
		name     string
		routes   map[string]string
		wantType string
	}{
		{
			name: "device target",
			routes: map[string]string{
				"/api/dcim/devices/": `{"results":[{"id":18,"name":"sw-01"}]}`,
			},
			wantType: "changed_object_type=dcim.device",
		},
		{
			name: "virtual machine target",
			routes: map[string]string{
				"/api/dcim/devices/":                    `{"results":[]}`,
				"/api/virtualization/virtual-machines/": `{"results":[{"id":18,"name":"sw-01"}]}`,
			},
			wantType: "changed_object_type=virtualization.virtualmachine",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.routes["/api/core/object-changes/"] = `{"results":[],"next":null}`
			f := &fakeDoer{routes: c.routes}
			if _, err := newReader(t, f).Read(context.Background(), "sw-01",
				time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC), time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)); err != nil {
				t.Fatalf("read: %v", err)
			}
			var changelog string
			for _, u := range f.seen {
				if strings.HasPrefix(u, "/api/core/object-changes/") {
					changelog = u
				}
			}
			if changelog == "" {
				t.Fatal("the changelog was never queried")
			}
			if !strings.Contains(changelog, c.wantType) {
				t.Errorf("the changelog query does not scope to the resolved class.\nwant %q in: %s\n"+
					"Without it an id-only filter collects any object class sharing that number — and this "+
					"estate's changelog is dominated by a sync plugin with its own id space.",
					c.wantType, changelog)
			}
		})
	}
}

// A target that is NEITHER must fail closed and say so honestly — not silently return no evidence, which
// would read as "nobody touched this host" rather than "TG cannot see this host".
func TestAnUnknownTargetFailsClosedAndNamesBothClasses(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{
		"/api/dcim/devices/":                    `{"results":[]}`,
		"/api/virtualization/virtual-machines/": `{"results":[]}`,
	}}
	_, err := newReader(t, f).Read(context.Background(), "not-in-cmdb", time.Now().Add(-time.Hour), time.Now())
	if err == nil {
		t.Fatal("an unknown target returned no error — absence of evidence would be read as evidence of absence")
	}
	for _, want := range []string{"device", "virtual machine", "fail closed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q, so an operator cannot tell WHICH lookups were "+
				"tried: %v", want, err)
		}
	}
}

// Order is load-bearing: a name present in BOTH classes must resolve as the device. The estate has physical
// kit and guests, and silently preferring the VM would attribute a switch's changes to a guest.
func TestADeviceWinsOverASameNamedVirtualMachine(t *testing.T) {
	f := &fakeDoer{routes: map[string]string{
		"/api/dcim/devices/":                    `{"results":[{"id":7,"name":"dual"}]}`,
		"/api/virtualization/virtual-machines/": `{"results":[{"id":99,"name":"dual"}]}`,
		"/api/core/object-changes/":             `{"results":[],"next":null}`,
	}}
	if _, err := newReader(t, f).Read(context.Background(), "dual", time.Now().Add(-time.Hour), time.Now()); err != nil {
		t.Fatalf("read: %v", err)
	}
	joined := strings.Join(f.seen, "\n")
	if strings.Contains(joined, "/api/virtualization/virtual-machines/") {
		t.Error("the VM list was queried even though the device matched — the device lookup must short-circuit")
	}
	if !strings.Contains(joined, "changed_object_id=7") {
		t.Errorf("resolved the wrong id: %s", joined)
	}
}
