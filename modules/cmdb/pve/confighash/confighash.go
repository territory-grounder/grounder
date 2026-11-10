// Package confighash is the grounded positive observed-mutation SIGNAL for PVE guests (TG-466 slice 1):
// a per-guest CONFIG-hash diff against a persisted baseline, riding the estate refresh cadence.
//
// WHY CONFIG AND NOT LIFECYCLE STATE (the INV-09 rationale). TG-407's AttributeObserving escalates a
// covered-but-empty attribution to attributed-suspicious ONLY when the session carries a confirmed
// observed mutation. A lifecycle-STATE diff can never be that source: an organic crash IS a state
// change with no actor, so wiring it would flood attributed-suspicious and pause auto-heal — the exact
// forbidden failure the earlier review caught in alert-name heuristics. A guest's CONFIG does not
// change organically: the machine-managed keys that do are excluded from the hash (see volatileKeys),
// and what remains changes only when someone deliberately edits the guest. That is the signal.
//
// INDEPENDENT of the actor-evidence reader by design: modules/actorevidence/pve answers WHO from the
// PVE task log; this package answers WHETHER-A-MUTATION-HAPPENED from the config endpoint. The two
// must corroborate each other in slice 2, so neither derives from the other.
//
// READ-ONLY by construction (GET /cluster/resources + GET /nodes/{node}/{qemu|lxc}/{vmid}/config with
// the estate READ token as a core/config.SecretRef, INV-13 — never the actuation write token), and
// WIRED TO NOTHING in slice 1: no attribution caller, no worker composition-root registration. Slice 2
// (eval-gated, separate MR) hooks Collector.Sweep onto the estate refresh tick, registers Samples on
// /metrics, and threads the store's ChangedWithin read into AttributeInput.Observation.
package confighash

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/metrics"
)

// Doer is the minimal HTTP contract; *http.Client satisfies it and tests inject a fake PVE cluster.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Reader issues the two read-only PVE GETs this signal needs.
type Reader struct {
	base     string
	tokenRef config.SecretRef
	http     Doer
	timeout  time.Duration
}

// ReaderOption configures a Reader.
type ReaderOption func(*Reader)

// WithHTTPClient injects the transport (a fake in tests, an *http.Client in production — the caller
// supplies the TLS policy for self-signed PVE endpoints, same as the sibling estate source).
func WithHTTPClient(d Doer) ReaderOption { return func(r *Reader) { r.http = d } }

// WithTimeout bounds each request (with a compiled ceiling) so a hung PVE endpoint cannot stall the
// estate refresh it will ride.
func WithTimeout(d time.Duration) ReaderOption {
	return func(r *Reader) {
		if d > 0 && d <= 15*time.Second {
			r.timeout = d
		}
	}
}

// NewReader returns the config reader for a PVE base URL and a READ-ONLY token SecretRef (INV-13).
func NewReader(baseURL string, tokenRef config.SecretRef, opts ...ReaderOption) *Reader {
	r := &Reader{base: strings.TrimRight(baseURL, "/"), tokenRef: tokenRef, http: http.DefaultClient, timeout: 8 * time.Second}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Guest is one enumerated guest with everything the config GET needs to address it. Status is carried
// for logging only and is NEVER hashed — lifecycle state must not reach the signal (INV-09).
type Guest struct {
	VMID   int64
	Name   string
	Node   string
	Kind   string // "qemu" | "lxc"
	Status string
}

// ListGuests enumerates the cluster's guests from the SAME endpoint the estate refresh sweeps. A row
// missing its name, node, or vmid is skipped — a missing baseline is safer than a guessed one.
func (r *Reader) ListGuests(ctx context.Context) ([]Guest, error) {
	body, err := r.get(ctx, "/api2/json/cluster/resources?type=vm")
	if err != nil {
		return nil, err
	}
	// The data envelope is a POINTER on purpose (the sibling estate source's lesson): a 2xx JSON body
	// with no `data` key is a proxy/SSO/other-product answer, not an empty cluster, and decoding the two
	// identically would baseline an estate that was never read.
	var env struct {
		Data *[]struct {
			Type   string `json:"type"`
			Node   string `json:"node"`
			Name   string `json:"name"`
			Vmid   int64  `json:"vmid"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("confighash: malformed cluster resources: %w", err)
	}
	if env.Data == nil {
		return nil, fmt.Errorf("confighash: malformed cluster resources: no `data` envelope in %d bytes", len(body))
	}
	var out []Guest
	for _, row := range *env.Data {
		name, node := strings.TrimSpace(row.Name), strings.TrimSpace(row.Node)
		if name == "" || node == "" || row.Vmid <= 0 || (row.Type != "qemu" && row.Type != "lxc") {
			continue
		}
		out = append(out, Guest{VMID: row.Vmid, Name: name, Node: node, Kind: row.Type, Status: strings.TrimSpace(row.Status)})
	}
	return out, nil
}

// GuestConfig reads one guest's config and returns it normalized for hashing. The endpoint is the
// DEFAULT pending-applied view, deliberately: a pending change is itself a deliberate config-file
// write, and it surfaces here at QUEUE time — when the task log also carries its actor — rather than
// at apply time, which may coincide with an (organic) restart and would time-correlate a human edit
// with a lifecycle event the audit window no longer covers.
//
// An EMPTY config object is refused: a real guest config always carries keys (memory, cores, rootfs
// or a disk), so `{}` is a broken answer — baselining it would mint a false "changed" on the next
// good read, a fabricated mutation signal (fail closed, REQ-2307 direction: absent, never invented).
func (r *Reader) GuestConfig(ctx context.Context, g Guest) (map[string]string, error) {
	if g.Kind != "qemu" && g.Kind != "lxc" {
		return nil, fmt.Errorf("confighash: guest %q has unroutable kind %q (closed vocabulary)", g.Name, g.Kind)
	}
	body, err := r.get(ctx, fmt.Sprintf("/api2/json/nodes/%s/%s/%d/config", url.PathEscape(g.Node), g.Kind, g.VMID))
	if err != nil {
		return nil, err
	}
	var env struct {
		Data *map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("confighash: malformed guest config for %q: %w", g.Name, err)
	}
	if env.Data == nil {
		return nil, fmt.Errorf("confighash: malformed guest config for %q: no `data` envelope", g.Name)
	}
	if len(*env.Data) == 0 {
		return nil, fmt.Errorf("confighash: guest config for %q is empty — refusing to baseline a non-answer", g.Name)
	}
	return NormalizeGuestConfig(*env.Data), nil
}

func (r *Reader) get(ctx context.Context, path string) ([]byte, error) {
	token, err := r.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("confighash: read-only token unresolvable (INV-13): %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+token)
	req.Header.Set("Accept", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("confighash: GET %s → %d: %s", path, resp.StatusCode, msg)
	}
	return b, nil
}

// Collector is the sweep shape slice 2 hooks onto the estate refresh tick (the guest-liveness feed
// pattern): enumerate guests, hash each config, Record against the baseline store, tally. It is
// constructed and driven by TESTS ONLY in slice 1 — nothing in cmd/worker builds one yet.
type Collector struct {
	reader *Reader
	store  Baselines

	mu           sync.Mutex
	last         Report
	swept        bool
	changedTotal uint64
	erroredTotal uint64
}

// New returns a Collector over a Reader and a baseline store.
func New(reader *Reader, store Baselines) *Collector {
	return &Collector{reader: reader, store: store}
}

// changedGuestsCap bounds the names a Report carries — enough to name the movers in a log line,
// small enough that a mass re-baseline cannot flood one.
const changedGuestsCap = 8

// Report is one sweep's tally, denominators first (TG-365: publish the denominator beside the
// verdict): Swept counts guests whose config was read AND recorded; Errored counts guests where the
// read or the store failed — each of those minted NO signal (fail-closed) and is counted loudly,
// because a silently erroring sweep would otherwise be indistinguishable from a quiet estate.
type Report struct {
	Swept         int
	FirstSighted  int
	Changed       int
	Errored       int
	ChangedGuests []string
}

// Sweep refreshes every guest's baseline once. It returns an error ONLY when the enumeration itself
// failed (nothing was swept); per-guest failures degrade to Report.Errored so one broken guest never
// starves the rest of the estate of baselines.
func (c *Collector) Sweep(ctx context.Context) (Report, error) {
	guests, err := c.reader.ListGuests(ctx)
	if err != nil {
		return Report{}, err
	}
	var rep Report
	for _, g := range guests {
		cfg, err := c.reader.GuestConfig(ctx, g)
		if err != nil {
			rep.Errored++
			continue
		}
		out, err := c.store.Record(ctx, Observed{VMID: g.VMID, Guest: g.Name, Node: g.Node, Kind: g.Kind, Hash: HashConfig(cfg)})
		if err != nil {
			rep.Errored++
			continue
		}
		rep.Swept++
		if out.FirstSighting {
			rep.FirstSighted++
		}
		if changed, _ := out.Signal(); changed {
			rep.Changed++
			if len(rep.ChangedGuests) < changedGuestsCap {
				rep.ChangedGuests = append(rep.ChangedGuests, g.Name)
			}
		}
	}
	c.mu.Lock()
	c.last, c.swept = rep, true
	c.changedTotal += uint64(rep.Changed)
	c.erroredTotal += uint64(rep.Errored)
	c.mu.Unlock()
	return rep, nil
}

// Samples publishes the tg_pve_confighash_* metrics for the /metrics registry (slice 2 registers it).
// Before the first completed sweep it emits NOTHING — honest absence, never a phantom zero-guest
// estate (the estate-doc coverage discipline).
func (c *Collector) Samples() []metrics.Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.swept {
		return nil
	}
	return []metrics.Sample{
		{Name: "tg_pve_confighash_guests", Kind: metrics.Gauge, Value: float64(c.last.Swept),
			Help: "guests whose config hash was baselined in the last sweep (TG-466)"},
		{Name: "tg_pve_confighash_changed_total", Kind: metrics.Counter, Value: float64(c.changedTotal),
			Help: "cumulative observed guest CONFIG changes vs baseline — deliberate acts by construction, the positive observed-mutation signal (TG-466)"},
		{Name: "tg_pve_confighash_errored_total", Kind: metrics.Counter, Value: float64(c.erroredTotal),
			Help: "cumulative per-guest sweep failures; each minted NO signal (fail-closed) — a rising value means the signal is starving, not that the estate is quiet (TG-466)"},
	}
}
