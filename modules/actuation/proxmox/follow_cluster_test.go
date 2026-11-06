package proxmox

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// TG-623: the "heal ALL guests" scope was a static name list that silently missed guests created later.
// Follow-cluster admits any guest that resolves to exactly one NON-TEMPLATE cluster guest at attempt time.

func followCluster() *fakeDoer {
	return &fakeDoer{
		resources: `[{"name":"web","node":"pve01","vmid":101,"type":"lxc","status":"running"},` +
			`{"name":"brandnew","node":"pve04","vmid":104101202,"type":"qemu","status":"stopped","template":0},` +
			`{"name":"tmpl-debian","node":"pve04","vmid":9000,"type":"qemu","status":"stopped","template":1},` +
			`{"name":"twin","node":"pve03","vmid":201,"type":"lxc","status":"stopped"},` +
			`{"name":"twin","node":"pve04","vmid":202,"type":"lxc","status":"stopped"}]`,
		upid:       "UPID:pve04:0000ABCD:00001234:60ABCDEF:qmstart:104101202:root@pam:",
		exitstatus: "OK",
	}
}

func TestFollowClusterAdmitsAnUnlistedNonTemplateGuest(t *testing.T) {
	f := followCluster()
	m := New(testBaseURL, tokenRef(t), WithHTTPClient(f),
		WithMutation(safety.NewActuatingChokepoint(), []string{"web"}), WithFollowCluster())
	if !m.FollowsCluster() {
		t.Fatal("WithFollowCluster on a mutation-configured module must report FollowsCluster")
	}
	res, err := m.Exec(context.Background(), []string{"start", "brandnew"}, nil)
	if err != nil {
		t.Fatalf("TG-623: a guest created after the allowlist was written must be healable under follow-cluster: %v", err)
	}
	if res.ExitCode != 0 || !f.sawPath("/api2/json/nodes/pve04/qemu/104101202/status/start") {
		t.Fatalf("must start the resolved qemu guest, exit=%d saw %v", res.ExitCode, f.seenURLs)
	}
}

func TestFollowClusterNeverAdmitsATemplate(t *testing.T) {
	f := followCluster()
	m := New(testBaseURL, tokenRef(t), WithHTTPClient(f),
		WithMutation(safety.NewActuatingChokepoint(), []string{"web"}), WithFollowCluster())
	if _, err := m.Exec(context.Background(), []string{"start", "tmpl-debian"}, nil); !errors.Is(err, ErrGuestNotAllowed) {
		t.Fatalf("follow-cluster must refuse a template with ErrGuestNotAllowed, got %v", err)
	}
	if f.sawStatusPost() {
		t.Fatalf("a refused template must never see a status POST, saw %v", f.seenURLs)
	}
}

func TestFollowClusterKeepsExactlyOneMatch(t *testing.T) {
	f := followCluster()
	m := New(testBaseURL, tokenRef(t), WithHTTPClient(f),
		WithMutation(safety.NewActuatingChokepoint(), nil), WithFollowCluster())
	for _, name := range []string{"twin", "no-such-guest"} {
		if _, err := m.Exec(context.Background(), []string{"start", name}, nil); err == nil {
			t.Fatalf("follow-cluster must still fail closed on an ambiguous or absent name (%q)", name)
		}
	}
	if f.sawStatusPost() {
		t.Fatalf("no ambiguous/absent guest may reach a status POST, saw %v", f.seenURLs)
	}
}

func TestFollowClusterWithoutAMutationProviderStillDeniesEverything(t *testing.T) {
	f := followCluster()
	m := New(testBaseURL, tokenRef(t), WithHTTPClient(f), WithFollowCluster())
	if m.FollowsCluster() {
		t.Fatal("follow-cluster on a module with no allowlist provider must not report active — it cannot arm actuation")
	}
	if _, err := m.Exec(context.Background(), []string{"start", "brandnew"}, nil); err == nil {
		t.Fatal("a module with no mutation wiring must refuse even under follow-cluster")
	}
	if len(f.seenURLs) != 0 {
		t.Fatalf("a read-only module must not touch the network, saw %v", f.seenURLs)
	}
}

func TestFollowClusterOffKeepsTheStaticAllowlist(t *testing.T) {
	f := followCluster()
	m := New(testBaseURL, tokenRef(t), WithHTTPClient(f), WithMutation(safety.NewActuatingChokepoint(), []string{"web"}))
	if _, err := m.Exec(context.Background(), []string{"start", "brandnew"}, nil); !errors.Is(err, ErrGuestNotAllowed) {
		t.Fatalf("without WithFollowCluster an unlisted guest is refused exactly as before, got %v", err)
	}
	if len(f.seenURLs) != 0 {
		t.Fatalf("static mode must refuse an unlisted name BEFORE any network call, saw %v", f.seenURLs)
	}
}

// A followed guest gets exactly the verbs a listed guest gets. stop is start-guest's commit-confirm inverse:
// refusing it on a followed guest would leave every heal of that guest un-revertable. The never-auto floor
// still refuses the power-cycle family with no network call.
func TestFollowClusterGrantsTheListedVerbsAndKeepsTheFloor(t *testing.T) {
	f := followCluster()
	f.upid = "UPID:pve04:0000ABCD:00001234:60ABCDEF:qmstop:104101202:root@pam:"
	m := New(testBaseURL, tokenRef(t), WithHTTPClient(f),
		WithMutation(safety.NewActuatingChokepoint(), nil), WithFollowCluster())
	if _, err := m.Exec(context.Background(), []string{"stop", "brandnew"}, nil); err != nil {
		t.Fatalf("the commit-confirm inverse (stop) of a followed guest must stay executable: %v", err)
	}
	if !f.sawPath("/api2/json/nodes/pve04/qemu/104101202/status/stop") {
		t.Fatalf("stop must reach the resolved guest, saw %v", f.seenURLs)
	}
	f2 := followCluster()
	m2 := New(testBaseURL, tokenRef(t), WithHTTPClient(f2),
		WithMutation(safety.NewActuatingChokepoint(), nil), WithFollowCluster())
	for _, op := range []string{"reboot", "shutdown", "reset", "destroy"} {
		if _, err := m2.Exec(context.Background(), []string{op, "brandnew"}, nil); err == nil {
			t.Fatalf("follow-cluster must not lift the never-auto floor (%s)", op)
		}
	}
	if len(f2.seenURLs) != 0 {
		t.Fatalf("a floor op must be refused before any network call, saw %v", f2.seenURLs)
	}
}
