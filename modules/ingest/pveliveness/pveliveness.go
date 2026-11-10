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
//   - FOLLOW-CLUSTER (TG-623, opt-in via WithFollowCluster): the watched set is the allowlist PLUS every
//     non-template lxc/qemu guest the cluster reports on THIS poll. A guest created after boot is watched from
//     its first observation (prior="" ⇒ its first stop after that is an edge; an already-stopped new guest
//     never fires). Templates are never watched: they cannot run, so they have no liveness. Off by default ⇒
//     byte-identical to the static-allowlist behaviour.
//   - EXCLUSIVE GROUPS (TG-654, opt-in via WithExclusiveGroups): a declared operator swap on a RAM-tight node
//     (core/guestgroup) is POLICY, not a fault. A group member's stop edge mints NOTHING at the edge. Instead a
//     per-group STATE check runs after every successful poll: the default member stopped while no partner runs
//     arms a grace window; a partner running or the default coming back clears it; the window elapsing raises
//     ONE guest-down for the default ("the swap did not complete"), which then takes the normal heal path back to
//     the declared default. A partner's own stop is never raised (its default is OFF). This is the one place the
//     detector reasons over state rather than an edge, and it is bounded to the declared members.
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
	"github.com/territory-grounder/grounder/core/guestgroup"
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
	// Template is 1 for a template (an unstartable image). FOLLOW-CLUSTER never watches one; the static
	// allowlist path ignores the field, so an operator who names a template keeps today's behaviour.
	Template int `json:"template"`
}

// Source polls Proxmox guest liveness and mints a triage envelope on each running→stopped transition of a
// managed (allowlisted) guest. Construct with New.
type Source struct {
	baseURL  string
	tokenRef config.SecretRef
	allowed  map[string]bool // guest NAMES TG manages; empty ⇒ fire for none (fail-safe) unless followCluster
	// followCluster widens the watched set to every non-template guest the cluster reports, per poll (TG-623).
	followCluster bool
	site          string
	http          Doer
	now           func() time.Time

	// Exclusive groups (TG-654, WithExclusiveGroups). groups nil ⇒ none declared: byte-identical behaviour.
	groups []guestgroup.Group
	grace  time.Duration
	logf   func(string, ...any)

	mu         sync.Mutex                 // guards prior + lastStates/statesSeen + excl
	prior      map[string]string          // guest name → last-observed status (for edge detection)
	lastStates []GuestState               // watched guests' states from the last SUCCESSFUL fetch (the TG-496 projection feed)
	statesSeen bool                       // false until the first successful fetch — keeps GuestStates honest (unknown, not empty)
	excl       map[string]*exclusiveCheck // group default member → its armed check (TG-654)
}

// exclusiveCheck is one armed exclusive-group check: the default member has been stopped, with no partner
// running, since `since`. raised marks that this episode already minted its one guest-down.
type exclusiveCheck struct {
	since  time.Time
	raised bool
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

// WithFollowCluster makes the watched set the allowlist PLUS every non-template lxc/qemu guest the cluster
// reports on each poll, so a guest created after boot is covered without an allowlist edit (TG-623: the
// "watch ALL guests" ruling was encoded as a static name list and silently missed three guests created later).
func WithFollowCluster() Option { return func(s *Source) { s.followCluster = true } }

// FollowsCluster reports whether this source watches every non-template cluster guest (WithFollowCluster).
func (s *Source) FollowsCluster() bool { return s.followCluster }

// WithExclusiveGroups declares exclusive guest groups (TG-654, core/guestgroup): a member's stop is the declared
// swap, not a fault, and the default member is raised only if it stays down with no partner running for longer
// than grace. logf receives one line per policy decision (deferred, expected, raised); nil discards them. An
// empty group list, or a grace ≤ 0, leaves the detector byte-identical.
func WithExclusiveGroups(groups []guestgroup.Group, grace time.Duration, logf func(string, ...any)) Option {
	return func(s *Source) {
		if len(groups) == 0 || grace <= 0 {
			return
		}
		s.groups = groups
		s.grace = grace
		s.logf = logf
		if s.logf == nil {
			s.logf = func(string, ...any) {}
		}
		s.excl = make(map[string]*exclusiveCheck)
	}
}

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
	if len(s.allowed) == 0 && !s.followCluster {
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
	// Exclusive groups read members' status whether or not they are watched: a partner outside the watched
	// set still decides whether the default's stop is the declared swap (TG-654). When a name appears on more
	// than one row (a same-named template, a guest elsewhere), a NOT-stopped row wins: for a partner that is
	// the conservative reading (it may be running), and for the default it means "do not raise".
	var statusNow map[string]resourcesRow
	if s.groups != nil {
		statusNow = make(map[string]resourcesRow, len(rows))
		for _, r := range rows {
			if r.Type != "lxc" && r.Type != "qemu" {
				continue
			}
			name := strings.TrimSpace(r.Name)
			if have, ok := statusNow[name]; ok && !isStopped(have.Status) {
				continue
			}
			statusNow[name] = r
		}
	}
	for _, r := range rows {
		if r.Type != "lxc" && r.Type != "qemu" {
			continue
		}
		name := strings.TrimSpace(r.Name)
		if name == "" || !s.watches(name, r) {
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
			if g, role := s.exclusiveRole(name); role != guestgroup.RoleNone {
				// TG-654: a group member's stop is decided by the post-poll sweep below, never at the edge.
				if role == guestgroup.RolePartner {
					s.logf("exclusive group %s: partner %s stopped — a partner's default is OFF, so it is never raised (TG-654)", g, name)
				} else {
					s.logf("exclusive group %s: %s stopped — the group check decides, not this edge (TG-654)", g, name)
				}
				continue
			}
			if env, ok := s.envelopeFor(name, r, now); ok {
				out = append(out, env)
			}
		}
	}
	out = append(out, s.sweepExclusive(statusNow, now)...)
	// Commit the fresh watched-guest states on THIS successful fetch only (a failed fetch returned above and
	// never reaches here). GuestStates() now reports ok=true and returns these — the projection feed.
	s.lastStates, s.statesSeen = states, true
	return out, nil
}

// isStopped reports a guest status that is exactly "stopped" (trimmed, any case): the only reading the
// exclusive-group rules accept as "off".
func isStopped(status string) bool { return strings.EqualFold(strings.TrimSpace(status), "stopped") }

// exclusiveRole returns the declared exclusive group a guest belongs to and its role (TG-654); RoleNone when no
// group is declared or the guest is in none.
func (s *Source) exclusiveRole(name string) (guestgroup.Group, guestgroup.Role) {
	if s.groups == nil {
		return guestgroup.Group{}, guestgroup.RoleNone
	}
	return guestgroup.Lookup(s.groups, name)
}

// sweepExclusive runs each group's state check over this poll's statuses and returns the guest-downs to raise.
// Called with s.mu held, after a SUCCESSFUL fetch only.
//
// Per group: a running partner means the swap is in its valid OFF/ON state, and the check clears (logged once
// if it had been armed). The default running means the default state is restored, and the check clears. The
// default stopped with no partner running arms the check; once it has stayed so for grace, the default is
// raised ONCE as a guest-down ("the swap did not complete"), which the normal heal path answers by restoring
// the declared default. A default that is absent from the cluster, or not watched, is never raised.
func (s *Source) sweepExclusive(statusNow map[string]resourcesRow, now time.Time) []coreingest.IncidentEnvelope {
	if s.groups == nil {
		return nil
	}
	var out []coreingest.IncidentEnvelope
	for _, g := range s.groups {
		def, present := statusNow[g.Default]
		// A partner counts as possibly running unless it reads exactly "stopped": `unknown` (its node is down or
		// partitioned) and `paused` (still holding its RAM) are not evidence that it is off. The same rule the
		// actuator refuses on, so the two planes never disagree about one row. An absent partner is off.
		running := ""
		for _, p := range g.Partners {
			if pr, ok := statusNow[p]; ok && !isStopped(pr.Status) {
				running = p
				break
			}
		}
		chk := s.excl[g.Default]
		switch {
		case running != "":
			if chk != nil {
				s.logf("exclusive group %s: %s is stopped while partner %s is not stopped — the declared swap, not a fault; no triage (TG-654)", g, g.Default, running)
				delete(s.excl, g.Default)
			}
		case !present || !isStopped(def.Status):
			delete(s.excl, g.Default) // default running (or unknown): nothing to check
		case chk == nil:
			s.excl[g.Default] = &exclusiveCheck{since: now}
			s.logf("exclusive group %s: %s is stopped and no partner runs — waiting up to %s for the declared swap before raising it (TG-654)", g, g.Default, s.grace)
		case !chk.raised && now.Sub(chk.since) >= s.grace:
			chk.raised = true
			if !s.watches(g.Default, def) {
				s.logf("exclusive group %s: %s has been stopped for %s with no partner running, but it is not watched — not raised (TG-654)", g, g.Default, now.Sub(chk.since).Round(time.Second))
				continue
			}
			summary := fmt.Sprintf("guest %s (vmid %d on %s) observed STOPPED for %s while no exclusive-group partner (%s) runs: the declared swap did not complete (TG-654)",
				g.Default, def.Vmid, def.Node, now.Sub(chk.since).Round(time.Second), strings.Join(g.Partners, ", "))
			if env, ok := s.envelopeWith(g.Default, def, now, summary); ok {
				out = append(out, env)
				s.logf("exclusive group %s: %s stopped for %s with no partner running — raised as a guest-down (TG-654)", g, g.Default, now.Sub(chk.since).Round(time.Second))
			}
		}
	}
	return out
}

// watches reports whether this guest is in the watched set: on the allowlist, or (follow-cluster) any
// non-template guest the cluster reported this poll.
func (s *Source) watches(name string, r resourcesRow) bool {
	if s.allowed[name] {
		return true
	}
	return s.followCluster && r.Template == 0
}

// WatchedCount is the size of the watched set as of the last SUCCESSFUL fetch: the liveness yield's
// denominator. Under follow-cluster the configured allowlist length is not the denominator, so the caller
// reads this after every poll instead.
func (s *Source) WatchedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lastStates)
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
	return s.envelopeWith(name, r, now, fmt.Sprintf("guest %s (vmid %d on %s) observed STOPPED by TG PVE liveness poller", name, r.Vmid, r.Node))
}

// envelopeWith is envelopeFor with the summary supplied (the exclusive-group raise names why it fired).
func (s *Source) envelopeWith(name string, r resourcesRow, now time.Time, summary string) (coreingest.IncidentEnvelope, bool) {
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
	raw.Site = s.siteFor(r.Node)
	raw.Summary = summary
	raw.ObservedAt = now
	env, err := coreingest.Normalize(raw, now)
	if err != nil {
		return coreingest.IncidentEnvelope{}, false
	}
	return env, true
}

// siteFor is the site a guest-down belongs to: the site of the guest's OWNING NODE when the node's name is
// a declared site spelling (core/ingest CanonicalizeSite folds "dc2pve01" to dc2), else the configured
// site. One Proxmox cluster spans both sites, and stamping every guest with the one configured site recorded
// every GR outage as NL (TG-631, found in the TG-624 G3 soak): per-site scoring, the site-keyed diagnostics
// deployment and every site join then aimed at the wrong site. An unrecognised node name keeps the
// configured site, so an estate whose node names carry no site loses nothing.
func (s *Source) siteFor(node string) string {
	if c := coreingest.CanonicalizeSite(node); coreingest.IsDeclaredSite(c) {
		return c
	}
	return s.site
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
