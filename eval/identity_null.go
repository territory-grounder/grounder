package eval

// TG-176 — offline identity-token null-control harness.
//
// This harness re-runs already-scored sessions under THREE arms and measures how far the agent's
// verdict (op-class, band, confidence, proposed argv) DRIFTS when a session's identity tokens —
// hostnames and role tags — are stripped of meaning. It answers one falsifiable question: is a
// verdict driven by GROUNDED STATE (tool observations, estate topology) or by the CONNOTATIONS of a
// hostname/role tag ("fw" ⇒ firewall ⇒ be cautious; "gpu" ⇒ expensive ⇒ don't touch)? A well-grounded
// agent's verdict is INVARIANT under the null transforms; drift is the identity-connotation leak.
//
// It is the NAME-surface analogue of the estate degree-preserving ShuffledControl (core/estate/
// control.go, INV-22 / eval/falsifiability.go): the same "beat a degree-preserving null or you encode
// no signal" discipline, pointed at identity tokens instead of graph edges. The shuffle mirrors that
// file's deterministic seeding convention exactly — a splitmix64 PRNG seeded from a session/plan hash,
// never math/rand, never wall-clock — so the control is replay-stable for a given seed.
//
// Offline by construction: the verdict SOURCE is the VerdictFunc seam. Re-deriving a real verdict
// needs the live agent loop (agent.Agent.Run over the model gateway), so the unit test injects a
// DETERMINISTIC mock VerdictFunc and proves scrub/shuffle/drift-scoring with NO live LLM or estate.
// A full drift MEASUREMENT over the real corpus with the real brain wires a live VerdictFunc (see
// LiveVerdictFunc doc) and is a later SUPERVISED run, kept out of CI.

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// Arm names one replay arm.
type Arm string

const (
	ArmReal     Arm = "real"     // the unmodified baseline session
	ArmScrubbed Arm = "scrubbed" // every identity token → an opaque stable id (semantics destroyed)
	ArmShuffled Arm = "shuffled" // identity tokens permuted across hosts (distribution kept, assignment destroyed)
)

// Verdict is the identity-independent decision surface of one replayed session — the four facets
// TG-176 measures for drift, mirroring the fields the eval Session / agent.Result already carry:
// op-class (proposal.Action.OpClass), band (Session.Band), confidence (proposal.Confidence), and the
// proposed argv (a deterministic serialization of manifest.Action, see ArgvOf). Proposed records
// whether the agent proposed at all (a grounded stand-down carries an empty op-class/argv).
type Verdict struct {
	Proposed   bool     `json:"proposed"`
	OpClass    string   `json:"op_class"`
	Band       string   `json:"band"`
	Confidence float64  `json:"confidence"`
	Argv       []string `json:"argv,omitempty"`
}

// IdentityInput is the identity-bearing input surface of one session: the tokens whose meaning the
// null-control destroys. Hosts and RoleTags are the two token namespaces the ticket names; Sites is
// the third semantic namespace (site prefixes such as "dc1"/"dc1" that also encode locality
// and are embedded inside hostnames). Summary is free text that may embed any of them. Host is the
// alert's primary host and MUST appear in Hosts. Everything a verdict could key on that leaks a
// hostname/role connotation lives here.
type IdentityInput struct {
	Ref       string   `json:"ref"`
	Host      string   `json:"host"`
	Site      string   `json:"site"`
	AlertRule string   `json:"alert_rule"`
	Summary   string   `json:"summary"`
	Hosts     []string `json:"hosts"`     // every hostname token referenced (incl. Host)
	RoleTags  []string `json:"role_tags"` // every role/type token (fw, gpu, prod, ...)
	Sites     []string `json:"sites"`     // every site token (dc1, dc1, dc2, ...)
}

// Render concatenates the identity-bearing surface into the text a verdict function reads. The
// null-control test greps Render(scrubbed) to assert no semantic token survives. Ref is excluded — it
// is a correlation key (ADR-0010), not an identity token, and is never rewritten.
func (in IdentityInput) Render() string {
	return strings.Join([]string{
		in.Host, in.Site, in.AlertRule, in.Summary,
		strings.Join(in.Hosts, " "), strings.Join(in.RoleTags, " "), strings.Join(in.Sites, " "),
	}, "\n")
}

// VerdictFunc produces the acting model's verdict for one (possibly identity-transformed) input. In
// production it wraps the live agent loop; in the offline unit test it is a deterministic mock. This
// is the seam that keeps the harness CI-runnable with no live LLM or estate.
//
// LiveVerdictFunc (the supervised corpus run, NOT wired into CI): render the IdentityInput into an
// incident seed, run agent.Agent.Run over the model gateway, then fill Verdict from the Result —
// OpClass/Argv from res.Proposal.Action (via ArgvOf), Band from the gate classification, Confidence
// from res.Confidence, Proposed from res.Outcome. That wrapper imports agent/model; this harness
// deliberately does not, so the delivered unit is unambiguously offline.
type VerdictFunc func(in IdentityInput) Verdict

// ArgvOf builds the canonical, deterministic proposed-argv for a verdict from an action's fields
// (manifest.Action: Target, Op, OpClass, Params). Params are emitted key-sorted so the argv is stable
// — the same action always yields the same argv, so argv DRIFT means the proposal changed, not the
// map iteration order. The live VerdictFunc uses this to fill Verdict.Argv from proposal.Action.
func ArgvOf(target, op, opClass string, params map[string]string) []string {
	argv := []string{target, op, opClass}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, k+"="+params[k])
	}
	return argv
}

// IdentityMap records the exact token rewrite one arm applied — the reproducibility artefact the
// ticket requires: any surprising drift replays deterministically from this map. It is old→new over
// each namespace. For ArmReal every namespace map is nil (the identity transform). JSON-serializable
// so a run can persist it beside the drift row.
type IdentityMap struct {
	Arm   Arm               `json:"arm"`
	Seed  string            `json:"seed,omitempty"`
	Hosts map[string]string `json:"hosts,omitempty"`
	Roles map[string]string `json:"roles,omitempty"`
	Sites map[string]string `json:"sites,omitempty"`
}

// ScrubMap builds the scrubbed arm's map: every identity token → an opaque STABLE id, assigned in
// deterministic (sorted) order so the mapping is reproducible and referentially consistent (the same
// token always maps to the same id). Host names → host-0001.., role tags → role-A.., site tokens →
// site-1.. — NO semantic fragment of the original survives.
func ScrubMap(in IdentityInput) IdentityMap {
	m := IdentityMap{Arm: ArmScrubbed, Hosts: map[string]string{}, Roles: map[string]string{}, Sites: map[string]string{}}
	for i, h := range uniqSorted(in.Hosts) {
		m.Hosts[h] = fmt.Sprintf("host-%04d", i+1)
	}
	for i, r := range uniqSorted(in.RoleTags) {
		m.Roles[r] = "role-" + opaqueRoleID(i)
	}
	for i, s := range uniqSorted(in.Sites) {
		m.Sites[s] = fmt.Sprintf("site-%d", i+1)
	}
	return m
}

// ShuffleMap builds the shuffled arm's map: within each namespace the tokens are PERMUTED (a
// degree-preserving reassignment — the multiset of names is identical, only the host→name assignment
// is destroyed, so a firewall may carry a GPU host's name). Deterministic under seed: a splitmix64
// PRNG seeded from the session/plan hash drives a Fisher–Yates permutation, mirroring
// estate.ShuffledControl's convention exactly. Re-running with the same seed yields the same
// permutation; NO math/rand, NO wall-clock. A namespace of <2 distinct tokens can only map to itself
// (a documented degeneracy: one name cannot be de-correlated from itself) — with ≥2 tokens the
// permutation is a genuine reassignment for the overwhelming majority of seeds, and the drift/RED
// tests pin a seed that moves the token under test.
func ShuffleMap(in IdentityInput, seed string) IdentityMap {
	return IdentityMap{
		Arm:   ArmShuffled,
		Seed:  seed,
		Hosts: permuteNamespace(uniqSorted(in.Hosts), seed+"|hosts"),
		Roles: permuteNamespace(uniqSorted(in.RoleTags), seed+"|roles"),
		Sites: permuteNamespace(uniqSorted(in.Sites), seed+"|sites"),
	}
}

// permuteNamespace maps each token to another token drawn from the SAME multiset via a seeded
// Fisher–Yates shuffle of a sorted copy — distribution preserved, assignment destroyed.
func permuteNamespace(toks []string, seed string) map[string]string {
	out := make(map[string]string, len(toks))
	perm := append([]string(nil), toks...)
	rng := newNullRand(seed)
	for i := len(perm) - 1; i > 0; i-- {
		j := int(rng.next() % uint64(i+1))
		perm[i], perm[j] = perm[j], perm[i]
	}
	for i, t := range toks {
		out[t] = perm[i]
	}
	return out
}

// Apply rewrites every identity token in the input under this map, returning the transformed input.
// Referential consistency is structural: one old→new function is applied uniformly to every field, so
// the same host maps to the same id everywhere in the session. The single-pass replacer replaces
// long tokens (hostnames, site prefixes) by descending length — so a hostname is consumed whole
// before its embedded site prefix could match — and short role tags only on word boundaries, so a
// two-letter tag never corrupts an unrelated substring. It never re-scans a replacement, so the
// shuffle's A→B, B→A swaps do not chain.
func (m IdentityMap) Apply(in IdentityInput) IdentityInput {
	rr := m.rules()
	rw := func(s string) string { return replaceTokens(s, rr) }
	rwAll := func(in []string) []string {
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = rw(s)
		}
		return out
	}
	out := in
	out.Host = rw(in.Host)
	out.Site = rw(in.Site)
	out.AlertRule = rw(in.AlertRule)
	out.Summary = rw(in.Summary)
	out.Hosts = rwAll(in.Hosts)
	out.RoleTags = rwAll(in.RoleTags)
	out.Sites = rwAll(in.Sites)
	return out
}

type replRule struct {
	from, to string
	word     bool // require word boundaries (short role tags, so "fw" never matches inside a word)
}

// rules flattens the map into a single length-descending, tie-broken rule list. Sorting makes the
// pass reproducible regardless of Go map iteration order, and longest-first guarantees a hostname is
// matched before an embedded site prefix.
func (m IdentityMap) rules() []replRule {
	var rr []replRule
	for o, n := range m.Hosts {
		rr = append(rr, replRule{o, n, false})
	}
	for o, n := range m.Sites {
		rr = append(rr, replRule{o, n, false})
	}
	for o, n := range m.Roles {
		rr = append(rr, replRule{o, n, true})
	}
	sort.Slice(rr, func(i, j int) bool {
		if len(rr[i].from) != len(rr[j].from) {
			return len(rr[i].from) > len(rr[j].from)
		}
		return rr[i].from < rr[j].from
	})
	return rr
}

// replaceTokens performs a single left-to-right pass, at each index taking the longest matching rule
// (rules are pre-sorted longest-first) and emitting its replacement WITHOUT re-scanning the emitted
// text. That single property gives three correctness guarantees at once: referential consistency
// (uniform old→new), longest-match (hostname before embedded site prefix), and swap-safety (a
// shuffle's A→B, B→A never chains into A→B→A).
func replaceTokens(s string, rules []replRule) string {
	if len(rules) == 0 || s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		matched := false
		for _, r := range rules {
			n := len(r.from)
			if n == 0 || i+n > len(s) || s[i:i+n] != r.from {
				continue
			}
			if r.word && !wordBoundary(s, i, i+n) {
				continue
			}
			b.WriteString(r.to)
			i += n
			matched = true
			break
		}
		if !matched {
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// wordBoundary reports whether [start,end) in s is delimited by non-word bytes (or string edges) on
// both sides — so a short role tag ("fw") is rewritten only as a standalone token, never inside a
// longer word.
func wordBoundary(s string, start, end int) bool {
	if start > 0 && isWordByte(s[start-1]) {
		return false
	}
	if end < len(s) && isWordByte(s[end]) {
		return false
	}
	return true
}

// ArmResult is one arm's replay: the transform applied (its map, the reproducibility artefact), the
// transformed input, and the resulting verdict.
type ArmResult struct {
	Arm     Arm           `json:"arm"`
	Map     IdentityMap   `json:"map"`
	Input   IdentityInput `json:"input"`
	Verdict Verdict       `json:"verdict"`
}

// ReplayArms runs one session under all three arms with vf and returns each arm's result keyed by arm.
// seed drives the shuffle permutation (mirror estate.ShuffledControl: a session/plan hash). The real
// arm applies no transform, so its verdict is the baseline the two null arms are scored against.
func ReplayArms(base IdentityInput, seed string, vf VerdictFunc) map[Arm]ArmResult {
	out := map[Arm]ArmResult{}
	for _, m := range []IdentityMap{{Arm: ArmReal}, ScrubMap(base), ShuffleMap(base, seed)} {
		in := m.Apply(base)
		out[m.Arm] = ArmResult{Arm: m.Arm, Map: m, Input: in, Verdict: vf(in)}
	}
	return out
}

// confidenceDriftEpsilon: any confidence change beyond float noise counts as drift. The harness's job
// is to surface identity-driven confidence movement, so the threshold is deliberately near-zero.
const confidenceDriftEpsilon = 1e-9

// VerdictDrift is the per-facet drift of one arm's verdict from the real (baseline) verdict, plus an
// aggregate in [0,1]. A facet flag is true when that facet moved under the identity transform; the
// aggregate is the mean of the FOUR facet flags the ticket names (op-class, band, confidence, argv).
// 0 = identity-invariant (grounded); 1 = every facet moved (verdict driven by identity connotation).
type VerdictDrift struct {
	Arm               Arm     `json:"arm"`
	OpClassChanged    bool    `json:"op_class_changed"`
	BandChanged       bool    `json:"band_changed"`
	ConfidenceDelta   float64 `json:"confidence_delta"`
	ConfidenceChanged bool    `json:"confidence_changed"`
	ArgvChanged       bool    `json:"argv_changed"`
	ProposedChanged   bool    `json:"proposed_changed"` // a propose⇄stand-down flip; reported, not in the aggregate
	Aggregate         float64 `json:"aggregate"`
}

// DriftOf scores one arm's verdict against the real baseline.
func DriftOf(arm Arm, real, got Verdict) VerdictDrift {
	d := VerdictDrift{
		Arm:             arm,
		OpClassChanged:  real.OpClass != got.OpClass,
		BandChanged:     real.Band != got.Band,
		ConfidenceDelta: math.Abs(real.Confidence - got.Confidence),
		ArgvChanged:     !argvEqual(real.Argv, got.Argv),
		ProposedChanged: real.Proposed != got.Proposed,
	}
	d.ConfidenceChanged = d.ConfidenceDelta > confidenceDriftEpsilon
	changed := 0
	for _, f := range []bool{d.OpClassChanged, d.BandChanged, d.ConfidenceChanged, d.ArgvChanged} {
		if f {
			changed++
		}
	}
	d.Aggregate = float64(changed) / 4
	return d
}

func argvEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ReplayCorpus is one session queued for replay: its identity input and the seed for its shuffle.
type ReplayCorpus struct {
	Input IdentityInput
	Seed  string
}

// DriftRow is the per-session drift under both null arms, carrying the scrub + shuffle maps so a
// surprising row is reproducible/inspectable (the ticket's "record the map" requirement).
type DriftRow struct {
	Ref        string       `json:"ref"`
	Real       Verdict      `json:"real"`
	Scrubbed   VerdictDrift `json:"scrubbed"`
	Shuffled   VerdictDrift `json:"shuffled"`
	ScrubMap   IdentityMap  `json:"scrub_map"`
	ShuffleMap IdentityMap  `json:"shuffle_map"`
}

// ArmDriftSummary aggregates one null arm's drift across the replayed corpus.
type ArmDriftSummary struct {
	Arm                 Arm     `json:"arm"`
	MeanAggregate       float64 `json:"mean_aggregate"`      // mean per-session aggregate drift [0,1]
	OpClassDriftRate    float64 `json:"op_class_drift_rate"` // fraction of sessions whose op-class moved
	BandDriftRate       float64 `json:"band_drift_rate"`
	ConfidenceDriftMean float64 `json:"confidence_drift_mean"` // mean |Δconfidence|
	ArgvDriftRate       float64 `json:"argv_drift_rate"`
	ProposedDriftRate   float64 `json:"proposed_drift_rate"`
}

// IdentityNullAgg is the grounding-scorecard DIMENSION TG-176 contributes: how far verdicts drift
// under each identity null across the corpus, plus a single headline GroundingStability number. It
// mirrors FalsifiabilityAgg's scorecard shape (a per-run struct with a headline ratio). Higher
// stability is better: 1.0 = verdicts are identity-invariant (driven by grounded state); toward 0 =
// verdicts track hostname/role connotation, the pathology this dimension exists to expose.
type IdentityNullAgg struct {
	N                  int             `json:"n"`
	Scrubbed           ArmDriftSummary `json:"scrubbed"`
	Shuffled           ArmDriftSummary `json:"shuffled"`
	GroundingStability float64         `json:"grounding_stability"` // 1 - max(scrubbed, shuffled) mean aggregate drift
}

// ScoreIdentityNull replays every session under all three arms with vf, scores per-session drift, and
// aggregates into the scorecard dimension. Pure and deterministic given a deterministic vf — the whole
// harness runs in CI over fixtures with a mock vf; a live vf is the later supervised corpus run.
func ScoreIdentityNull(corpus []ReplayCorpus, vf VerdictFunc) ([]DriftRow, IdentityNullAgg) {
	rows := make([]DriftRow, 0, len(corpus))
	agg := IdentityNullAgg{N: len(corpus)}
	agg.Scrubbed.Arm, agg.Shuffled.Arm = ArmScrubbed, ArmShuffled
	for _, c := range corpus {
		arms := ReplayArms(c.Input, c.Seed, vf)
		real := arms[ArmReal].Verdict
		sd := DriftOf(ArmScrubbed, real, arms[ArmScrubbed].Verdict)
		hd := DriftOf(ArmShuffled, real, arms[ArmShuffled].Verdict)
		rows = append(rows, DriftRow{
			Ref: c.Input.Ref, Real: real, Scrubbed: sd, Shuffled: hd,
			ScrubMap: arms[ArmScrubbed].Map, ShuffleMap: arms[ArmShuffled].Map,
		})
		accumDrift(&agg.Scrubbed, sd)
		accumDrift(&agg.Shuffled, hd)
	}
	if n := float64(len(corpus)); n > 0 {
		finalizeDrift(&agg.Scrubbed, n)
		finalizeDrift(&agg.Shuffled, n)
	}
	agg.GroundingStability = 1 - math.Max(agg.Scrubbed.MeanAggregate, agg.Shuffled.MeanAggregate)
	return rows, agg
}

func accumDrift(s *ArmDriftSummary, d VerdictDrift) {
	s.MeanAggregate += d.Aggregate
	s.ConfidenceDriftMean += d.ConfidenceDelta
	if d.OpClassChanged {
		s.OpClassDriftRate++
	}
	if d.BandChanged {
		s.BandDriftRate++
	}
	if d.ArgvChanged {
		s.ArgvDriftRate++
	}
	if d.ProposedChanged {
		s.ProposedDriftRate++
	}
}

func finalizeDrift(s *ArmDriftSummary, n float64) {
	s.MeanAggregate /= n
	s.ConfidenceDriftMean /= n
	s.OpClassDriftRate /= n
	s.BandDriftRate /= n
	s.ArgvDriftRate /= n
	s.ProposedDriftRate /= n
}

// KnownRoleTags are the estate's conventional role/type tokens IncidentToInput recognises when
// bridging a corpus Incident into the identity surface for the live run. It is a CONVENTION list, not
// an exhaustive taxonomy — the offline killing test authors tokens explicitly and never relies on it.
var KnownRoleTags = []string{"fw", "gpu", "prod", "dev", "db", "k8s", "lb", "cache"}

// sitePrefixRe matches the estate's leading site prefix (two-to-six lowercase letters + two digits,
// e.g. "dc1", "dc2") at the head of a hostname.
var sitePrefixRe = regexp.MustCompile(`^[a-z]{2,6}[0-9]{2}`)

// IncidentToInput bridges an eval corpus Incident (corpus.json) into the harness's identity surface
// for the LIVE supervised run. It extracts identity tokens from the estate naming CONVENTION — the
// leading site prefix and any known role tag embedded in the host — plus the incident Site. It is
// best-effort by design: the full peer set (blast-radius neighbours the agent reads at run time) is
// added by the live VerdictFunc, not here. The offline killing test does NOT use this; it authors
// IdentityInputs with explicitly known tokens.
func IncidentToInput(inc Incident) IdentityInput {
	in := IdentityInput{
		Ref: inc.ExternalRef, Host: inc.Host, Site: inc.Site,
		AlertRule: inc.AlertRule, Summary: inc.Summary,
		Hosts: []string{inc.Host},
	}
	for _, r := range KnownRoleTags {
		if strings.Contains(inc.Host, r) {
			in.RoleTags = append(in.RoleTags, r)
		}
	}
	sites := []string{}
	if p := sitePrefixRe.FindString(inc.Host); p != "" {
		sites = append(sites, p)
	}
	if inc.Site != "" {
		sites = append(sites, inc.Site)
	}
	in.Sites = uniqSorted(sites)
	return in
}

// uniqSorted returns the sorted, de-duplicated, non-empty tokens of in.
func uniqSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// opaqueRoleID yields A..Z then a numeric fallback for role scrub ids (matches the ticket's "role-A"
// style). More than 26 distinct role tags in one session is not expected; the fallback keeps ids
// unique and deterministic rather than pretty.
func opaqueRoleID(i int) string {
	if i < 26 {
		return string(rune('A' + i))
	}
	return fmt.Sprintf("N%d", i+1)
}

// nullRand mirrors core/estate/control.go's detRand — a splitmix64 PRNG seeded from a string via
// FNV-1a, with no global math/rand, so the shuffle control is replay-stable for a given seed. It is
// reproduced here (rather than imported) because estate's copy is unexported; mirroring the CONVENTION
// is the ticket's explicit ask.
type nullRand struct{ state uint64 }

func newNullRand(seed string) *nullRand {
	var h uint64 = 1469598103934665603 // FNV-1a offset basis
	for i := 0; i < len(seed); i++ {
		h ^= uint64(seed[i])
		h *= 1099511628211 // FNV-1a prime
	}
	return &nullRand{state: h}
}

func (r *nullRand) next() uint64 {
	r.state += 0x9E3779B97F4A7C15
	z := r.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}
