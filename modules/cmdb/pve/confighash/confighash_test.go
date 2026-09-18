package confighash

// TG-466 slice 1. THE NEGATIVE DRILL IS THE POINT of this suite: an ORGANIC lifecycle change (crash,
// stop, start — config untouched) and the machine's own backup-window config-file scribbles (lock,
// digest, parent) must yield changed=false, while a deliberate config EDIT yields changed=true. That
// asymmetry IS the INV-09 safety property — the signal downstream escalates covered-but-empty
// attributions to attributed-suspicious (POLL_PAUSE + security escalation), so an organic event that
// could move it would flood SECURITY and neuter auto-heal.
//
// KILLING MUTATIONS (executed 2026-08-14, each restored to green):
//   - Outcome.Signal: drop the FirstSighting branch (return o.Changed || o.FirstSighting semantics via
//     `return true, o.PreviousHash` on first sighting) → TestOrganicLifecycleDrill fails at sweep 1
//     ("first sighting must not read as a mutation") and TestFirstSightingIsNotAChange fails.
//   - HashConfig: stop excluding volatileKeys (`if false && volatileKeys[k]`) → the drill fails at the
//     backup-noise sweep ("organic/machine event read as a mutation") and
//     TestHashConfigExcludesVolatileKeys fails.
//   - Diff: return `true, ""` on store error → TestStoreErrorFailsClosed fails ("a store error must
//     never fabricate a mutation").

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// memStore mirrors the guest_config_baseline semantics in memory (the db store proper is exercised
// db-gated in core/db). fail simulates a broken persistence layer.
type memStore struct {
	mu   sync.Mutex
	fail error
	rows map[int64]struct{ hash, prev string }
}

func newMemStore() *memStore {
	return &memStore{rows: map[int64]struct{ hash, prev string }{}}
}

func (m *memStore) Record(_ context.Context, obs Observed) (Outcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return Outcome{}, m.fail
	}
	r, ok := m.rows[obs.VMID]
	if !ok {
		m.rows[obs.VMID] = struct{ hash, prev string }{hash: obs.Hash}
		return Outcome{FirstSighting: true}, nil
	}
	if r.hash == obs.Hash {
		return Outcome{PreviousHash: r.hash}, nil
	}
	m.rows[obs.VMID] = struct{ hash, prev string }{hash: obs.Hash, prev: r.hash}
	return Outcome{Changed: true, PreviousHash: r.hash}, nil
}

// fakePVE is a mutable route-mapped cluster: RequestURI → body (or a non-200 status override).
type fakePVE struct {
	mu     sync.Mutex
	routes map[string]string
	status map[string]int
}

func (f *fakePVE) set(uri, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[uri] = body
	delete(f.status, uri)
}

func (f *fakePVE) setStatus(uri string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[uri] = code
}

func (f *fakePVE) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	uri := req.URL.RequestURI()
	if code, ok := f.status[uri]; ok {
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("boom")), Header: make(http.Header)}, nil
	}
	body, ok := f.routes[uri]
	if !ok {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("no route " + uri)), Header: make(http.Header)}, nil
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

const (
	resourcesURI = "/api2/json/cluster/resources?type=vm"
	webConfigURI = "/api2/json/nodes/pve-a/lxc/201/config"
	dbConfigURI  = "/api2/json/nodes/pve-b/qemu/202/config"
)

// resourcesBody renders the cluster listing with per-guest power states — the ONLY thing the organic
// drill flips. Shapes mirror live /cluster/resources rows (identifiers neutralized).
func resourcesBody(webStatus, dbStatus string) string {
	return `{"data":[
		{"type":"lxc","node":"pve-a","name":"web01","vmid":201,"status":"` + webStatus + `"},
		{"type":"qemu","node":"pve-b","name":"db01","vmid":202,"status":"` + dbStatus + `"},
		{"type":"storage","node":"pve-a","name":"local-zfs"},
		{"type":"lxc","node":"","name":"unplaced","vmid":209},
		{"type":"lxc","node":"pve-a","name":"","vmid":210},
		{"type":"lxc","node":"pve-a","name":"no-vmid"}
	]}`
}

// Config fixtures are modeled on LIVE configs read from a real cluster on 2026-08-14 (mixed JSON
// types: LXC memory is a NUMBER, QEMU memory is a STRING; LXC carries the raw `lxc` array), with
// identifiers neutralized (RFC5737 addresses, locally-administered MACs).
const webConfigV1 = `{"data":{
	"hostname":"web01","arch":"amd64","cores":2,"memory":4096,"swap":512,
	"rootfs":"local-zfs:subvol-201-disk-0,size=8G",
	"net0":"name=eth0,bridge=vmbr0,gw=192.0.2.1,hwaddr=02:00:00:00:02:01,ip=192.0.2.10/24,type=veth",
	"features":"nesting=1","onboot":1,"ostype":"debian","tags":" ",
	"lxc":[["lxc.cgroup2.devices.allow","a"],["lxc.mount.entry","/dev/net dev/net none bind,create=dir"]],
	"digest":"aaa111"
}}`

// webConfigEdited is a DELIBERATE act: memory raised, a second NIC added (and PVE's digest moved with
// the file, as it does). This is the one sweep that must fire.
const webConfigEdited = `{"data":{
	"hostname":"web01","arch":"amd64","cores":2,"memory":8192,"swap":512,
	"rootfs":"local-zfs:subvol-201-disk-0,size=8G",
	"net0":"name=eth0,bridge=vmbr0,gw=192.0.2.1,hwaddr=02:00:00:00:02:01,ip=192.0.2.10/24,type=veth",
	"net1":"name=eth1,bridge=vmbr1,hwaddr=02:00:00:00:02:0A,ip=dhcp,type=veth",
	"features":"nesting=1","onboot":1,"ostype":"debian","tags":" ",
	"lxc":[["lxc.cgroup2.devices.allow","a"],["lxc.mount.entry","/dev/net dev/net none bind,create=dir"]],
	"digest":"ddd444"
}}`

const dbConfigV1 = `{"data":{
	"name":"db01","cores":4,"sockets":1,"cpu":"host","memory":"8192","machine":"q35",
	"scsi0":"local-zfs:vm-202-disk-0,discard=on,size=64G","scsihw":"virtio-scsi-pci",
	"net0":"virtio=02:00:00:00:02:02,bridge=vmbr0","boot":"order=scsi0","ostype":"l26",
	"onboot":1,"agent":"1","smbios1":"uuid=00000000-0000-4000-8000-000000000202",
	"vmgenid":"00000000-0000-4000-8000-000000000203",
	"meta":"creation-qemu=9.0.2,ctime=1700000000","digest":"bbb222"
}}`

// dbConfigBackupWindow is the MACHINE mid-scheduled-backup: vzdump wrote lock into the config file,
// the snapshot machinery moved parent, PVE pinned runningmachine, and digest moved because the FILE
// moved. No human anywhere — the hash must not move either.
const dbConfigBackupWindow = `{"data":{
	"name":"db01","cores":4,"sockets":1,"cpu":"host","memory":"8192","machine":"q35",
	"scsi0":"local-zfs:vm-202-disk-0,discard=on,size=64G","scsihw":"virtio-scsi-pci",
	"net0":"virtio=02:00:00:00:02:02,bridge=vmbr0","boot":"order=scsi0","ostype":"l26",
	"onboot":1,"agent":"1","smbios1":"uuid=00000000-0000-4000-8000-000000000202",
	"vmgenid":"00000000-0000-4000-8000-000000000203",
	"meta":"creation-qemu=9.0.2,ctime=1700000000",
	"lock":"backup","parent":"vzdump-tmp","runningmachine":"pc-q35-9.0","digest":"ccc333"
}}`

func newFixture(t *testing.T) (*fakePVE, *Collector, *memStore) {
	t.Helper()
	t.Setenv("TG_TEST_PVE_CH_TOKEN", "tg-estate@pve!ro=uuid")
	f := &fakePVE{routes: map[string]string{}, status: map[string]int{}}
	f.set(resourcesURI, resourcesBody("running", "running"))
	f.set(webConfigURI, webConfigV1)
	f.set(dbConfigURI, dbConfigV1)
	r := NewReader("https://pve-a:8006/", config.SecretRef("env:TG_TEST_PVE_CH_TOKEN"), WithHTTPClient(f))
	st := newMemStore()
	return f, New(r, st), st
}

// TestOrganicLifecycleDrill is the INV-09 drill the ticket demands, end to end through the real
// Reader→Hash→Record path: crash/stop/start and backup-window noise are NOT mutations; an edit IS.
func TestOrganicLifecycleDrill(t *testing.T) {
	ctx := context.Background()
	f, c, st := newFixture(t)

	// Sweep 1 — first sighting establishes baselines and is NOT a change (no baseline existed to diff).
	rep, err := c.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if rep.Swept != 2 || rep.FirstSighted != 2 || rep.Changed != 0 || rep.Errored != 0 {
		t.Fatalf("sweep 1: first sighting must not read as a mutation: %+v", rep)
	}

	// Sweep 2 — ORGANIC: web01 crashes (status running→stopped, config byte-identical) while db01 sits
	// mid-scheduled-backup (lock/parent/runningmachine/digest scribbled by the machine). ZERO changes —
	// this is the sweep that, red, would flood attributed-suspicious and pause auto-heal.
	f.set(resourcesURI, resourcesBody("stopped", "running"))
	f.set(dbConfigURI, dbConfigBackupWindow)
	rep, err = c.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if rep.Changed != 0 || rep.Errored != 0 {
		t.Fatalf("INV-09 VIOLATED: an organic/machine event read as a mutation: %+v", rep)
	}

	// Sweep 3 — web01 starts again, db01's backup finished (its config file reverts). Still zero.
	f.set(resourcesURI, resourcesBody("running", "running"))
	f.set(dbConfigURI, dbConfigV1)
	rep, err = c.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep 3: %v", err)
	}
	if rep.Changed != 0 {
		t.Fatalf("INV-09 VIOLATED: a stop/start cycle read as a mutation: %+v", rep)
	}

	// Sweep 4 — a DELIBERATE config edit on web01. Exactly one change, correctly attributed to web01,
	// with the previous baseline preserved for the slice-2 evidence trail.
	f.set(webConfigURI, webConfigEdited)
	rep, err = c.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep 4: %v", err)
	}
	if rep.Changed != 1 || len(rep.ChangedGuests) != 1 || rep.ChangedGuests[0] != "web01" {
		t.Fatalf("a config EDIT must read as exactly one mutation on web01: %+v", rep)
	}
	row := st.rows[201]
	if row.prev == "" || row.prev == row.hash || !strings.HasPrefix(row.prev, hashScheme) {
		t.Fatalf("the edit must preserve the previous baseline hash: %+v", row)
	}

	// Sweep 5 — nothing moved since the edit: the signal fires ONCE per change, not per sweep.
	rep, err = c.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep 5: %v", err)
	}
	if rep.Changed != 0 {
		t.Fatalf("a change must fire once, not on every sweep after it: %+v", rep)
	}
}

func TestHashConfigStableOrderIndependentAndDiscriminating(t *testing.T) {
	a := map[string]string{"cores": "2", "memory": "4096", "net0": "name=eth0"}
	// Determinism, over a SEPARATELY-built identical map — not `HashConfig(a) != HashConfig(a)`, which
	// compares one call's result to itself: an assertion that cannot fail (SA4000), the unfalsifiable-
	// oracle trap. a2 has the same content, b the same content in a different insertion order.
	a2 := map[string]string{"net0": "name=eth0", "cores": "2", "memory": "4096"}
	b := map[string]string{}
	for _, k := range []string{"net0", "cores", "memory"} { // different insertion order again
		b[k] = a[k]
	}
	if HashConfig(a) != HashConfig(a2) || HashConfig(a) != HashConfig(b) {
		t.Fatal("identical configs must hash identically regardless of construction order")
	}
	edited := map[string]string{"cores": "2", "memory": "8192", "net0": "name=eth0"}
	if HashConfig(a) == HashConfig(edited) {
		t.Fatal("a value edit must move the hash")
	}
	// Length-prefix framing: shifting a byte across a key/value boundary must not collide.
	x := map[string]string{"ab": "c"}
	y := map[string]string{"a": "bc"}
	if HashConfig(x) == HashConfig(y) {
		t.Fatal("frame ambiguity: key/value boundary shifts must produce distinct hashes")
	}
}

func TestHashConfigExcludesVolatileKeys(t *testing.T) {
	base := map[string]string{"cores": "2", "memory": "4096"}
	noisy := map[string]string{
		"cores": "2", "memory": "4096",
		"digest": "ccc333", "lock": "backup", "parent": "vzdump-tmp",
		"snapstate": "prepare", "snaptime": "1700000001",
		"runningmachine": "pc-q35-9.0", "runningcpu": "host",
		"vmstate": "local-zfs:vm-202-state", "vmstatestorage": "local-zfs",
	}
	if HashConfig(base) != HashConfig(noisy) {
		t.Fatal("machine-managed volatile keys must not move the hash (the backup-night flood, INV-09)")
	}
}

func TestNormalizeCollapsesJSONTypeProjection(t *testing.T) {
	// Measured live: QEMU memory arrives as the STRING "16384", LXC memory as the NUMBER 4096. The
	// text config file is the ground truth, so the projection must not be able to fake a change.
	var asString, asNumber map[string]json.RawMessage
	mustUnmarshal(t, `{"memory":"16384","onboot":1}`, &asString)
	mustUnmarshal(t, `{"memory":16384,"onboot":1}`, &asNumber)
	hs := HashConfig(NormalizeGuestConfig(asString))
	hn := HashConfig(NormalizeGuestConfig(asNumber))
	if hs != hn {
		t.Fatal("a JSON type projection flap (string vs number) must not read as a config change")
	}
	// Two INDEPENDENT decodes of the same JSON — comparing one decode to itself is the same SA4000
	// unfalsifiable oracle; the raw lxc array (a nested array PVE returns verbatim) must normalize
	// identically across separate decodes for the hash to be stable.
	const lxcArrayJSON = `{"lxc":[["lxc.cgroup2.devices.allow","a"],["lxc.mount.entry","x y none bind"]]}`
	var withArray, withArray2 map[string]json.RawMessage
	mustUnmarshal(t, lxcArrayJSON, &withArray)
	mustUnmarshal(t, lxcArrayJSON, &withArray2)
	if HashConfig(NormalizeGuestConfig(withArray)) != HashConfig(NormalizeGuestConfig(withArray2)) {
		t.Fatal("the raw lxc array must normalize deterministically")
	}
}

func TestFirstSightingIsNotAChange(t *testing.T) {
	ctx := context.Background()
	f, c, _ := newFixture(t)
	if _, err := c.Sweep(ctx); err != nil {
		t.Fatalf("baseline sweep: %v", err)
	}
	// A brand-new guest appears (deployed between sweeps): baseline it, never flag it.
	f.set(resourcesURI, `{"data":[
		{"type":"lxc","node":"pve-a","name":"web01","vmid":201,"status":"running"},
		{"type":"qemu","node":"pve-b","name":"db01","vmid":202,"status":"running"},
		{"type":"lxc","node":"pve-a","name":"new01","vmid":203,"status":"running"}
	]}`)
	f.set("/api2/json/nodes/pve-a/lxc/203/config", `{"data":{"hostname":"new01","cores":1,"memory":512}}`)
	rep, err := c.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.FirstSighted != 1 || rep.Changed != 0 {
		t.Fatalf("a newly-sighted guest is a baseline, not a mutation: %+v", rep)
	}
}

func TestReadErrorFailsClosedAndDoesNotPoisonTheBaseline(t *testing.T) {
	ctx := context.Background()
	f, c, _ := newFixture(t)
	if _, err := c.Sweep(ctx); err != nil {
		t.Fatalf("baseline sweep: %v", err)
	}
	// web01's config read breaks: NO signal is minted, the failure is COUNTED, others still sweep.
	f.setStatus(webConfigURI, 500)
	rep, err := c.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep with broken guest: %v", err)
	}
	if rep.Errored != 1 || rep.Changed != 0 || rep.Swept != 1 {
		t.Fatalf("a read error must fail closed (no signal), be counted, and starve nobody else: %+v", rep)
	}
	// The endpoint recovers with an IDENTICAL config: still no change — the error left the baseline
	// intact rather than blanking it into a false "changed" on recovery.
	f.set(webConfigURI, webConfigV1)
	rep, err = c.Sweep(ctx)
	if err != nil {
		t.Fatalf("recovery sweep: %v", err)
	}
	if rep.Changed != 0 || rep.Errored != 0 {
		t.Fatalf("recovery with an identical config must not read as a mutation: %+v", rep)
	}
	// And an edit made WHILE the read was broken is still caught after recovery.
	f.set(webConfigURI, webConfigEdited)
	rep, err = c.Sweep(ctx)
	if err != nil {
		t.Fatalf("post-recovery edit sweep: %v", err)
	}
	if rep.Changed != 1 {
		t.Fatalf("an edit across an outage must still be observed once: %+v", rep)
	}
}

func TestEmptyConfigObjectIsRefused(t *testing.T) {
	ctx := context.Background()
	f, c, st := newFixture(t)
	f.set(webConfigURI, `{"data":{}}`)
	rep, err := c.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.Errored != 1 {
		t.Fatalf("an empty config object is a non-answer and must be refused, not baselined: %+v", rep)
	}
	if _, ok := st.rows[201]; ok {
		t.Fatal("refusing must mean NOT baselining — an empty baseline would mint a false change on the next good read")
	}
}

func TestStoreErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	st.fail = errors.New("db down")
	if changed, prev := Diff(ctx, st, Observed{VMID: 201, Guest: "web01", Hash: "ch1:x"}); changed || prev != "" {
		t.Fatalf("a store error must never fabricate a mutation: changed=%v prev=%q", changed, prev)
	}
	f, c, _ := newFixture(t)
	c.store = st
	rep, err := c.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	_ = f
	if rep.Changed != 0 || rep.Errored != 2 {
		t.Fatalf("store failures must fail closed and be counted loudly: %+v", rep)
	}
}

func TestListGuestsRefusesNonPVEBodyAndSkipsUnroutableRows(t *testing.T) {
	ctx := context.Background()
	t.Setenv("TG_TEST_PVE_CH_TOKEN", "tg-estate@pve!ro=uuid")
	f := &fakePVE{routes: map[string]string{resourcesURI: `{"ok":true}`}, status: map[string]int{}}
	r := NewReader("https://pve-a:8006", config.SecretRef("env:TG_TEST_PVE_CH_TOKEN"), WithHTTPClient(f))
	if _, err := r.ListGuests(ctx); err == nil {
		t.Fatal("a 2xx body with no data envelope is not an empty cluster — it must be an error")
	}
	f.set(resourcesURI, resourcesBody("running", "running"))
	guests, err := r.ListGuests(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(guests) != 2 { // storage row, nodeless, nameless and vmid-less rows all skipped
		t.Fatalf("unroutable rows must be skipped, not guessed: %+v", guests)
	}
}

func TestSamplesHonestAbsenceThenValues(t *testing.T) {
	ctx := context.Background()
	f, c, _ := newFixture(t)
	if s := c.Samples(); s != nil {
		t.Fatalf("before any sweep the metrics must be ABSENT, not zero: %+v", s)
	}
	if _, err := c.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	f.set(webConfigURI, webConfigEdited)
	if _, err := c.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	byName := map[string]float64{}
	for _, s := range c.Samples() {
		byName[s.Name] = s.Value
	}
	if byName["tg_pve_confighash_guests"] != 2 || byName["tg_pve_confighash_changed_total"] != 1 || byName["tg_pve_confighash_errored_total"] != 0 {
		t.Fatalf("gauges must publish the sweep tallies: %+v", byName)
	}
}

func mustUnmarshal(t *testing.T, s string, into *map[string]json.RawMessage) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), into); err != nil {
		t.Fatalf("fixture unmarshal: %v", err)
	}
}
