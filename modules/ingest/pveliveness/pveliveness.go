// Package pveliveness is the loadable Proxmox-liveness ingest source (A1 detection latency, spec/008 kin).
//
// WHY. TG's push intake (LibreNMS) is fast to reason on but SLOW to fire: LibreNMS's device-down rules need a
// condition true for ~60s evaluated on ~300s SNMP polls, so a guest-down alert arrives ~6–11 min after the
// stop. Short-lived faults auto-restore before that window, so TG never sees them (measured A1 recall 61.9%).
// TG ALREADY reads Proxmox guest status directly (the same GET /cluster/resources the actuation lane resolves
// against). This source turns that read into a TG-NATIVE liveness detector: it polls guest status every
// ~30–60s and, on an observed running→stopped transition of a managed guest, mints ONE triage through the
// SAME core/ingest → StartTriage pipeline as a LibreNMS alert — beating the push pipeline by an order of
// magnitude, in one poll cycle.
//
// SAFETY / correctness invariants:
//   - READ-ONLY: GET /cluster/resources only; it never actuates. Mutation stays behind the mode chokepoint.
//   - EDGE-TRIGGERED: fires only on a running→stopped TRANSITION (prior=running observed), never on a guest
//     that is merely stopped — so a guest already down at startup does not storm, and a still-down guest does
//     not re-fire every tick. Temporal REJECT_DUPLICATE dedup is the second line of defense.
//   - NO self-actuation guard is needed: TG only ever STARTS a guest (start-guest heal) — it never stops one —
//     so a running→stopped transition is ALWAYS a real fault (injected or organic), never TG's own effect. An
//     up transition (a heal or an injection auto-restore) is not a down edge and is ignored by construction.
//   - Scoped to the operator allowlist (the guests TG manages): an empty allowlist fires for NOTHING
//     (fail-safe — no firing on unrelated/infra guests going down for maintenance).
//   - INV-04: the observed status is a claim validated into the one canonical envelope via core/ingest.Normalize.
//   - INV-13: the PVE token is a secret reference resolved at call time and never logged.
//
// Known, accepted redundancy: a fault the liveness poller catches fast will ALSO produce a slow LibreNMS push
// alert ~6–11 min later under a DIFFERENT external_ref. By then TG has usually healed the guest, so the late
// LibreNMS triage sees the guest already up and ages out as stale — redundant but harmless. Deduping across
// the two sources would require shared cross-source incident identity; deferred (the fast-detect A1/MTTR win
// is the point).
package pveliveness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/config"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// SourceType is the ingest source slug this module serves (mirrors librenms.SourceType).
const SourceType = "pve-liveness"

// DeviceDownRule is the alert-rule label minted for a guest-down transition. It intentionally matches the
// LibreNMS device-down precedent so the Runner classifies a liveness-sourced incident IDENTICALLY to a
// push-sourced one (no skill/prompt drift between the two intakes).
// DeviceDownRule is the alert_rule this detector stamps on every envelope it emits.
//
// EXPORTED because it is load-bearing for a measurement, not merely for a message: A1 (detection recall) is
// RULE-CLASS MATCHED, so if this string stops satisfying the device-down arm of detectRuleMatch
// (core/db/axis_read.go) then every guest-down this poller catches is written to the ingest ledger and STILL
// scores as a miss. The oracle in core/httpapi asserts against THIS constant so a change here is caught;
// asserting against a copied literal would test the test.
const DeviceDownRule = "Device-Down"

// Doer is the minimal HTTP contract (a fake in tests, a TLS-configured *http.Client in prod). It mirrors
// modules/ingest/librenms.Doer so the poller takes the same injected transport the estate uses.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// resourcesRow is one row of GET /api2/json/cluster/resources?type=vm (the subset this source consumes).
type resourcesRow struct {
	Type   string `json:"type"`   // "lxc" | "qemu"
	Node   string `json:"node"`   // the hypervisor node the guest is placed on
	Name   string `json:"name"`   // guest hostname
	Vmid   int    `json:"vmid"`   // guest id
	Status string `json:"status"` // "running" | "stopped" | …
}

// Source polls Proxmox guest liveness and mints a triage envelope on each running→stopped transition of a
// managed (allowlisted) guest. Construct with New.
type Source struct {
	baseURL  string
	tokenRef config.SecretRef
	allowed  map[string]bool // guest NAMES TG manages; empty ⇒ fire for none (fail-safe)
	site     string
	http     Doer
	now      func() time.Time

	mu         sync.Mutex        // guards prior + lastStates/statesSeen
	prior      map[string]string // guest name → last-observed status (for edge detection)
	lastStates []GuestState      // watched guests' states from the last SUCCESSFUL fetch (the TG-496 projection feed)
	statesSeen bool              // false until the first successful fetch — keeps GuestStates honest (unknown, not empty)
}

// GuestState is one watched guest's power state from the last successful FetchActive, in the hypervisor's
// vocabulary. It mirrors modules/cmdb/pve.GuestState by SHAPE but is kept MODULE-LOCAL on purpose: this
// ingest detector takes no dependency on the cmdb topology package (there is no other ingest→cmdb edge in
// the tree, and this source was built to depend only on core/config + core/ingest). The worker maps it into
// db.GuestLivenessState. ObservedAt is the fetch time, threaded into the projection's monotone upsert guard
// (TG-496) so this fast ~37s feed is not clobbered by the slower 5-min estate sweep during a down-transition.
type GuestState struct {
	Guest      string
	Node       string
	Status     string
	ObservedAt time.Time
}

// Option configures a Source.
type Option func(*Source)

// WithHTTPClient injects the HTTP transport (a fake in tests; estateHTTPClient in prod — PVE serves a
// self-signed cert, so prod passes the insecure-configured client the actuation lane also uses).
func WithHTTPClient(d Doer) Option { return func(s *Source) { s.http = d } }

// WithClock overrides the wall clock so transition timing + ObservedAt are deterministic under test.
func WithClock(now func() time.Time) Option { return func(s *Source) { s.now = now } }

// New builds a liveness source. baseURL + tokenRef are the SAME Proxmox endpoint/credential the actuation
// lane uses (TG_PROXMOX_BASE_URL / TG_PROXMOX_TOKEN_REF); allowedGuests is the operator allowlist
// (TG_PROXMOX_ALLOWED_GUESTS) — only these guests are watched, so the detector never fires on infra guests.
func New(baseURL, tokenRef string, allowedGuests []string, site string, opts ...Option) *Source {
	allowed := make(map[string]bool, len(allowedGuests))
	for _, g := range allowedGuests {
		if g = strings.TrimSpace(g); g != "" {
			allowed[g] = true
		}
	}
	s := &Source{
		baseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		tokenRef: config.SecretRef(tokenRef),
		allowed:  allowed,
		site:     strings.TrimSpace(site),
		http:     http.DefaultClient,
		now:      time.Now,
		prior:    make(map[string]string),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// FetchActive polls guest status once and returns a canonical envelope for every managed guest observed to
// have transitioned running→stopped SINCE THE LAST poll. It is read-only. A fetch/parse error returns no
// envelopes and the error (the caller logs + retries next tick). The prior-state map is updated under lock so
// each transition fires exactly once. The signature mirrors librenms.AlertSource.FetchActive minus the
// withheld count (this source has no min-age gate — a fresh down is always actionable).
func (s *Source) FetchActive(ctx context.Context) ([]coreingest.IncidentEnvelope, error) {
	if len(s.allowed) == 0 {
		return nil, nil // fail-safe: no allowlist ⇒ watch nothing
	}
	rows, err := s.fetchResources(ctx)
	if err != nil {
		return nil, err // failed fetch: cache untouched, GuestStates stays honest (never invent/refresh state)
	}
	now := s.now()
	var out []coreingest.IncidentEnvelope
	states := make([]GuestState, 0, len(s.allowed))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		if r.Type != "lxc" && r.Type != "qemu" {
			continue
		}
		name := strings.TrimSpace(r.Name)
		if name == "" || !s.allowed[name] {
			continue
		}
		// Cache EVERY watched guest's state (running AND stopped) from THIS fetch so the guest_liveness
		// projection can be refreshed at the detector's ~37s cadence, not only the 5-min estate sweep
		// (TG-496). Status/node are normalized exactly as the sibling writer modules/cmdb/pve.EstateSource
		// does (both feed the same table, latest-OBSERVED-wins); ObservedAt is this fetch's time for the
		// monotone guard. Only ALLOWLISTED guests are cached — the estate sweep remains the all-guests
		// backstop, and a guest this detector never watches ages out to unknown rather than being touched.
		states = append(states, GuestState{Guest: name, Node: strings.TrimSpace(r.Node), Status: strings.TrimSpace(r.Status), ObservedAt: now})
		prev := s.prior[name]
		s.prior[name] = r.Status
		// Fire ONLY on an observed running→stopped edge. unknown(prev="")→stopped is NOT a transition (a guest
		// already down when the poller started — leave it to the push backstop, do not storm on startup).
		if prev == "running" && r.Status == "stopped" {
			if env, ok := s.envelopeFor(name, r, now); ok {
				out = append(out, env)
			}
		}
	}
	// Commit the fresh watched-guest states on THIS successful fetch only (a failed fetch returned above and
	// never reaches here). GuestStates() now reports ok=true and returns these — the projection feed.
	s.lastStates, s.statesSeen = states, true
	return out, nil
}

// GuestStates returns the watched guests' power states cached by the last SUCCESSFUL FetchActive, and
// whether any fetch has succeeded yet. ok=false ⇒ no successful poll: the caller must write NOTHING to the
// projection (unknown, never an invented empty cluster — the same honesty contract pve.EstateSource keeps,
// TG-365). A failed fetch leaves the last good cache and its ok untouched. Only ALLOWLISTED guests appear
// here by construction; the reader's freshness bound (TG-378) retires an old reading, not this method.
func (s *Source) GuestStates() ([]GuestState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.statesSeen {
		return nil, false
	}
	out := make([]GuestState, len(s.lastStates))
	copy(out, s.lastStates)
	return out, true
}

// envelopeFor synthesizes and validates the canonical envelope for one guest-down transition. A guest whose
// name cannot form a valid slug is skipped (ok=false) rather than aborting the batch.
func (s *Source) envelopeFor(name string, r resourcesRow, now time.Time) (coreingest.IncidentEnvelope, bool) {
	// external_ref is unique per detection (guest + unix seconds): each down-transition is a distinct incident;
	// Temporal REJECT_DUPLICATE dedups a same-second re-fire. Sanitize the name to the slug alphabet so a guest
	// with an unusual character still yields a valid ref instead of being dropped by the validator.
	ref := "tg-liveness-" + slugify(name) + "-" + strconv.FormatInt(now.Unix(), 10)
	raw := coreingest.NewRawEvent(SourceType, nil)
	raw.SourceID = SourceType
	raw.ExternalRef = ref
	raw.AlertRule = DeviceDownRule
	raw.Severity = "critical" // a managed guest going down is service-affecting
	raw.Host = name
	raw.Site = s.site
	raw.Summary = fmt.Sprintf("guest %s (vmid %d on %s) observed STOPPED by TG PVE liveness poller", name, r.Vmid, r.Node)
	raw.ObservedAt = now
	env, err := coreingest.Normalize(raw, now)
	if err != nil {
		return coreingest.IncidentEnvelope{}, false
	}
	return env, true
}

// fetchResources issues the one authenticated read (GET /cluster/resources?type=vm) and decodes the rows.
func (s *Source) fetchResources(ctx context.Context) ([]resourcesRow, error) {
	token, err := s.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("pveliveness: resolve token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/api2/json/cluster/resources?type=vm", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("pveliveness: GET cluster/resources: status %d", resp.StatusCode)
	}
	var wrap struct {
		Data []resourcesRow `json:"data"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("pveliveness: malformed cluster/resources response: %w", err)
	}
	return wrap.Data, nil
}

// slugify maps a guest name into the external_ref slug alphabet ([A-Za-z0-9._-]); any other rune becomes '-'.
func slugify(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
