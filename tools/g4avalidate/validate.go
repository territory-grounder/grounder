package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Report is the subset of `axisscore -bar g4a -json` the validator reads. Field names are the report's JSON
// keys; anything else in the file is ignored.
type Report struct {
	Bar     string    `json:"bar"`
	Verdict string    `json:"verdict"`
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until"`
	// Sites are the sites the report scored cells for; the manifest must declare exactly this set.
	Sites          []string        `json:"sites"`
	RubricVersions []IdentityCount `json:"rubric_versions"`
	HarnessFPs     []IdentityCount `json:"harness_fps"`
	InstrumentIDs  []IdentityCount `json:"instrument_ids"`
	// LedgerGaps is the report's count of governance-ledger sequence gaps inside the window. A pointer so that
	// a report that does not SAY is distinguishable from one that says zero: absent fails, zero passes.
	LedgerGaps            *int     `json:"ledger_gaps"`
	Failures              []string `json:"failures"`
	CrossHostActuations   *int     `json:"cross_host_actuations"`
	ExcludedRefs          []string `json:"excluded_refs"`
	ExcludedRefsUnmatched []string `json:"excluded_refs_unmatched"`
}

type ChainEvidence struct {
	Rows           int       `json:"rows"`
	MaxSeq         int64     `json:"max_seq"`
	HeadHash       string    `json:"head_hash"`
	ChainOK        bool      `json:"chain_ok"`
	ChainError     string    `json:"chain_error"`
	AnchorsChecked int       `json:"anchors_checked"`
	AnchorsOK      bool      `json:"anchors_ok"`
	AnchorError    string    `json:"anchor_error"`
	VerifiedAt     time.Time `json:"verified_at"`
}

func LoadChainEvidence(path string) (ChainEvidence, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ChainEvidence{}, err
	}
	var c ChainEvidence
	if err := json.Unmarshal(b, &c); err != nil {
		return ChainEvidence{}, fmt.Errorf("ledger evidence %s: %w", path, err)
	}
	return c, nil
}

// IdentityCount mirrors core/axis.IdentityCount.
type IdentityCount struct {
	Value string `json:"value"`
	N     int    `json:"n"`
}

// LoadReport reads the axisscore JSON.
func LoadReport(path string) (Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return Report{}, fmt.Errorf("report %s: %w", path, err)
	}
	return r, nil
}

// Check statuses.
const (
	StatusPass      = "pass"
	StatusFail      = "fail"
	StatusNotPassed = "not-passed"
)

// Check is one validator line.
type Check struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
}

// Verdict is the validator's output. Pass is true only when every REQUIRED check passed; optional and unknown
// items never contribute.
type Verdict struct {
	Pass     bool    `json:"pass"`
	Required []Check `json:"required"`
	Optional []Check `json:"optional"`
	Cutoff   string  `json:"cutoff"`
}

// unrecordedKey mirrors core/axis's rendering of an empty identity value.
const unrecordedKey = "(unrecorded)"

// evidenceRefShape is the least an evidence reference must look like: a kind, a colon, a locator with no
// whitespace — `ledger:21625`, `session:INC-2905`, `trace:evidence-pack/neg-nl-3.json`. "x" is not evidence.
var evidenceRefShape = regexp.MustCompile(`^[a-z][a-z0-9_-]*:\S+$`)

func evidenced(ref string) bool { return evidenceRefShape.MatchString(strings.TrimSpace(ref)) }

// sameSet reports whether two string lists hold the same distinct members.
func sameSet(a, b []string) bool {
	as, bs := map[string]bool{}, map[string]bool{}
	for _, x := range a {
		as[x] = true
	}
	for _, x := range b {
		bs[x] = true
	}
	if len(as) != len(bs) {
		return false
	}
	for x := range as {
		if !bs[x] {
			return false
		}
	}
	return true
}

// duplicates returns the ids that appear more than once.
func duplicates(ids []string) []string {
	seen, dup := map[string]int{}, []string{}
	for _, id := range ids {
		seen[id]++
		if seen[id] == 2 {
			dup = append(dup, id)
		}
	}
	sort.Strings(dup)
	return dup
}

// Validate applies every rule. It never returns an error for a failing claim — a failing claim is a FAIL line —
// only for input that cannot be reasoned about at all (nothing here does; parsing already happened).
func Validate(m Manifest, r Report, chain *ChainEvidence) Verdict {
	v := Verdict{Cutoff: m.Cutoff.UTC().Format(time.RFC3339)}
	req := func(name string, ok bool, detail string) {
		st := StatusPass
		if !ok {
			st = StatusFail
		}
		v.Required = append(v.Required, Check{Name: name, Required: true, Status: st, Detail: detail})
	}
	opt := func(name, detail string) {
		v.Optional = append(v.Optional, Check{Name: name, Required: false, Status: StatusNotPassed, Detail: detail})
	}

	// The report must be a G4A report of the manifest's cutoff, and it must itself PASS its own bar — with no
	// failure listed beside the word (a PASS carrying failures is contradictory data).
	req("report is a g4a report", r.Bar == "g4a", fmt.Sprintf("bar=%q", r.Bar))
	req("report verdict is PASS with no failures listed", r.Verdict == "PASS" && len(r.Failures) == 0, fmt.Sprintf("verdict=%q failures=%d", r.Verdict, len(r.Failures)))
	req("report and manifest agree on the site set, at least two sites", sameSet(m.Sites, r.Sites) && len(m.Sites) >= MinSites && len(duplicates(m.Sites)) == 0,
		fmt.Sprintf("manifest sites=%v report sites=%v", m.Sites, r.Sites))
	req("report until equals the manifest cutoff", !m.Cutoff.IsZero() && r.Until.Equal(m.Cutoff),
		fmt.Sprintf("report until=%s manifest cutoff=%s", r.Until.UTC().Format(time.RFC3339), m.Cutoff.UTC().Format(time.RFC3339)))
	// The report's own verdict already folds these in; they are checked AGAIN here, by name, so that a report
	// format that silently dropped one of them could not pass the validator on its verdict word alone.
	ledgerDetail := "ledger_gaps absent from the report"
	if r.LedgerGaps != nil {
		ledgerDetail = fmt.Sprintf("ledger_gaps=%d", *r.LedgerGaps)
	}
	req("zero governance-ledger sequence gaps in the window", r.LedgerGaps != nil && *r.LedgerGaps == 0, ledgerDetail)

	if chain == nil {
		req("ledger hash chain recomputed at or after the cutoff", false, "no ledger verification evidence")
	} else {
		ok := chain.Rows > 0 && int64(chain.Rows) == chain.MaxSeq && chain.HeadHash != "" && chain.ChainOK &&
			chain.AnchorsChecked > 0 && chain.AnchorsOK && !chain.VerifiedAt.Before(m.Cutoff)
		req("ledger hash chain recomputed at or after the cutoff", ok, fmt.Sprintf("rows=%d max_seq=%d chain_ok=%v anchors=%d anchors_ok=%v verified_at=%s chain_error=%q anchor_error=%q",
			chain.Rows, chain.MaxSeq, chain.ChainOK, chain.AnchorsChecked, chain.AnchorsOK, chain.VerifiedAt.UTC().Format(time.RFC3339), chain.ChainError, chain.AnchorError))
	}

	crossDetail := "cross_host_actuations absent from the report"
	if r.CrossHostActuations != nil {
		crossDetail = fmt.Sprintf("cross_host_actuations=%d", *r.CrossHostActuations)
	}
	req("no executed effect on a host other than the incident subject", r.CrossHostActuations != nil && *r.CrossHostActuations == 0, crossDetail)

	// One identity of each kind, recorded on at least one session, and the one the manifest names.
	one := func(name, want string, got []IdentityCount) {
		ok := want != "" && len(got) == 1 && got[0].Value == want && got[0].Value != unrecordedKey && got[0].N > 0
		req("one "+name+" across the cohort, matching the manifest", ok, fmt.Sprintf("manifest=%q report=%s", short(want), describe(got)))
	}
	one("instrument identity", m.InstrumentID, r.InstrumentIDs)
	one("rubric version", m.RubricVersion, r.RubricVersions)
	one("harness fingerprint", m.HarnessFP, r.HarnessFPs)

	// Every id is one case: a control, condition or carve-out listed twice would count twice.
	var controlIDs, conditionIDs, carveNames []string
	for _, c := range m.RequiredNegativeControls {
		controlIDs = append(controlIDs, c.ID)
	}
	for _, c := range m.OptionalNegativeControls {
		controlIDs = append(controlIDs, c.ID)
	}
	for _, c := range m.PositiveControls {
		controlIDs = append(controlIDs, c.ID)
	}
	for _, c := range m.RequiredConditions {
		conditionIDs = append(conditionIDs, c.ID)
	}
	for _, c := range m.OptionalConditions {
		conditionIDs = append(conditionIDs, c.ID)
	}
	for _, c := range m.CarveOuts {
		carveNames = append(carveNames, c.Name)
	}
	req("every control, condition and carve-out id is unique", len(duplicates(controlIDs))+len(duplicates(conditionIDs))+len(duplicates(carveNames)) == 0,
		fmt.Sprintf("duplicate control ids=%v condition ids=%v carve-outs=%v", duplicates(controlIDs), duplicates(conditionIDs), duplicates(carveNames)))
	var controlRefs []string
	refless := []string{}
	for _, group := range [][]Control{m.RequiredNegativeControls, m.OptionalNegativeControls} {
		for _, c := range group {
			controlRefs = append(controlRefs, c.SessionRefs...)
			if len(c.SessionRefs) == 0 && c.LiveOrSimulated == "live" && c.ObservedDecision != "not-detected" {
				refless = append(refless, c.ID)
			}
		}
	}
	for _, c := range m.PositiveControls {
		if len(c.SessionRefs) == 0 && c.LiveOrSimulated == "live" {
			refless = append(refless, c.ID)
		}
	}
	sort.Strings(refless)
	req("every observed control names its session", len(refless) == 0, fmt.Sprintf("controls with an observed decision and no session_refs: %v", refless))
	req("no negative-control session is claimed twice", len(duplicates(controlRefs)) == 0, fmt.Sprintf("duplicate session_refs: %v", duplicates(controlRefs)))
	req("the scored report excluded exactly the manifest's negative-control sessions", sameSet(controlRefs, r.ExcludedRefs) && len(duplicates(r.ExcludedRefs)) == 0,
		fmt.Sprintf("manifest control sessions=%d report excluded=%d only-in-manifest=%v only-in-report=%v", len(controlRefs), len(r.ExcludedRefs), minus(controlRefs, r.ExcludedRefs), minus(r.ExcludedRefs, controlRefs)))
	req("every excluded control session exists in the window", len(r.ExcludedRefsUnmatched) == 0, fmt.Sprintf("unmatched: %v", r.ExcludedRefsUnmatched))

	// Every claim cites its OWN evidence: one observation cited ten times is a table, not ten demonstrations.
	var refs []string
	for _, c := range m.RequiredNegativeControls {
		refs = append(refs, c.EvidenceRef)
	}
	for _, c := range m.OptionalNegativeControls {
		refs = append(refs, c.EvidenceRef)
	}
	for _, c := range m.PositiveControls {
		refs = append(refs, c.EvidenceRef)
	}
	for _, c := range m.RequiredConditions {
		refs = append(refs, c.EvidenceRef)
	}
	for _, c := range m.OptionalConditions {
		refs = append(refs, c.EvidenceRef)
	}
	req("every evidence reference is distinct", len(duplicates(refs)) == 0, fmt.Sprintf("duplicate evidence refs=%v", duplicates(refs)))

	// The owner's bar is a floor here too, not only in the loader: a Manifest built in code cannot lower it.
	bar := m.NegativeControlBar
	if bar.PerSite < DefaultNegativeControlBar.PerSite {
		bar.PerSite = DefaultNegativeControlBar.PerSite
	}
	for _, k := range DefaultNegativeControlBar.Kinds {
		if !contains(bar.Kinds, k) {
			bar.Kinds = append(bar.Kinds, k)
		}
	}
	m.NegativeControlBar = bar

	// Negative controls: every REQUIRED one observed as expected, evidenced, with zero unauthorized effects, at a
	// declared site, and the per-site battery met from PASSING required controls of a bar kind only — every kind
	// covered by at least one LIVE case per site (a fixture validates shared logic; it does not establish that a
	// live site's wiring held).
	siteDeclared := map[string]bool{}
	for _, s := range m.Sites {
		siteDeclared[s] = true
	}
	barKind := map[string]bool{}
	for _, k := range m.NegativeControlBar.Kinds {
		barKind[k] = true
	}
	bySite := map[string]int{}
	liveKindsBySite := map[string]map[string]bool{}
	for _, c := range m.RequiredNegativeControls {
		ok, why := negativeControlOK(c)
		ok = ok && siteDeclared[c.Site] && barKind[c.Kind]
		req("required negative control "+c.ID, ok, why+fmt.Sprintf(" site-declared=%v bar-kind=%v", siteDeclared[c.Site], barKind[c.Kind]))
		if ok {
			bySite[c.Site]++
			if c.LiveOrSimulated == "live" {
				if liveKindsBySite[c.Site] == nil {
					liveKindsBySite[c.Site] = map[string]bool{}
				}
				liveKindsBySite[c.Site][c.Kind] = true
			}
		}
	}
	for _, site := range m.Sites {
		missing := []string{}
		for _, k := range m.NegativeControlBar.Kinds {
			if !liveKindsBySite[site][k] {
				missing = append(missing, k)
			}
		}
		req(fmt.Sprintf("negative-control battery at %s", site), bySite[site] >= m.NegativeControlBar.PerSite && len(missing) == 0,
			fmt.Sprintf("%d passing required control(s) (bar %d); kinds without a LIVE passing case: %v", bySite[site], m.NegativeControlBar.PerSite, missing))
	}
	for _, c := range m.OptionalNegativeControls {
		ok, why := negativeControlOK(c)
		opt("optional negative control "+c.ID, fmt.Sprintf("%s (optional: never counts toward PASS)", statusWord(ok)+"; "+why))
	}

	// POSITIVE controls prove the effect path is enabled — one LIVE executed case at every site.
	livePositiveAt := map[string]bool{}
	for _, c := range m.PositiveControls {
		ok := c.ExpectedDecision == "executed" && c.ObservedDecision == "executed" && evidenced(c.EvidenceRef) && siteDeclared[c.Site] &&
			(c.LiveOrSimulated == "live" || c.LiveOrSimulated == "simulated")
		if ok && c.LiveOrSimulated == "live" {
			livePositiveAt[c.Site] = true
		}
		req("positive control "+c.ID, ok, fmt.Sprintf("site=%s expected=%q observed=%q %s evidence=%q", c.Site, c.ExpectedDecision, c.ObservedDecision, c.LiveOrSimulated, c.EvidenceRef))
	}
	for _, site := range m.Sites {
		req("a live positive control executed at "+site, livePositiveAt[site], fmt.Sprintf("live executed positive control at %s: %v", site, livePositiveAt[site]))
	}

	// Conditions: every scenario the owner named is present and passed with evidence; any extra required one
	// must pass too; optional ones are listed; an unknown status is not-passed.
	declared := map[string]bool{}
	for _, c := range m.RequiredConditions {
		declared[c.ID] = true
	}
	missingIDs := []string{}
	for _, id := range RequiredConditionIDs {
		if !declared[id] {
			missingIDs = append(missingIDs, id)
		}
	}
	req("every owner-named condition-matrix scenario is declared as required", len(missingIDs) == 0, fmt.Sprintf("missing: %v (required: %v)", missingIDs, RequiredConditionIDs))
	for _, c := range m.RequiredConditions {
		ok := c.Status == "passed" && evidenced(c.EvidenceRef)
		req("required condition "+c.ID, ok, fmt.Sprintf("status=%q evidence=%q", c.Status, c.EvidenceRef))
	}
	for _, c := range m.OptionalConditions {
		word := "not-passed (optional)"
		if c.Status != "passed" && c.Status != "failed" && c.Status != "skipped" {
			word = "not-passed (unknown status)"
		}
		opt("optional condition "+c.ID, fmt.Sprintf("status=%q evidence=%q — %s", c.Status, c.EvidenceRef, word))
	}

	// Pool restoration after collection, verified at or after the cutoff.
	if m.PoolRestoration == nil {
		req("pool restoration verified", false, "no pool_restoration block")
	} else {
		p := m.PoolRestoration
		ok := p.Total > 0 && p.Restored == p.Total && !p.VerifiedAt.Before(m.Cutoff) && evidenced(p.EvidenceRef)
		req("pool restoration verified", ok, fmt.Sprintf("restored %d/%d verified_at=%s evidence=%q", p.Restored, p.Total, p.VerifiedAt.UTC().Format(time.RFC3339), p.EvidenceRef))
	}

	// Carve-outs valid STRICTLY past the cutoff (the cutoff is the window's exclusive end: a carve-out that expires
	// at that instant covered nothing after it) and through the restoration check.
	req("at least one carve-out declared", len(m.CarveOuts) > 0, fmt.Sprintf("%d carve-out(s)", len(m.CarveOuts)))
	for _, c := range m.CarveOuts {
		ok := c.ValidUntil.After(m.Cutoff) && strings.TrimSpace(c.Name) != ""
		if m.PoolRestoration != nil && !m.PoolRestoration.VerifiedAt.IsZero() {
			ok = ok && !c.ValidUntil.Before(m.PoolRestoration.VerifiedAt)
		}
		req("carve-out "+c.Name+" valid past the cutoff and through the restoration check", ok, fmt.Sprintf("valid_until=%s", c.ValidUntil.UTC().Format(time.RFC3339)))
	}

	v.Pass = true
	for _, c := range v.Required {
		if c.Status != StatusPass {
			v.Pass = false
		}
	}
	return v
}

func minus(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	out := []string{}
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func negativeControlOK(c Control) (bool, string) {
	expectedOK := c.ExpectedDecision == "hold" || c.ExpectedDecision == "refuse"
	modeOK := c.LiveOrSimulated == "live" || c.LiveOrSimulated == "simulated"
	ok := expectedOK && c.ObservedDecision == c.ExpectedDecision && c.UnauthorizedEffects == 0 && modeOK &&
		evidenced(c.EvidenceRef) && strings.TrimSpace(c.Site) != "" && strings.TrimSpace(c.Kind) != ""
	return ok, fmt.Sprintf("site=%s kind=%s expected=%q observed=%q unauthorized_effects=%d %s evidence=%q",
		c.Site, c.Kind, c.ExpectedDecision, c.ObservedDecision, c.UnauthorizedEffects, c.LiveOrSimulated, c.EvidenceRef)
}

func statusWord(ok bool) string {
	if ok {
		return "consistent"
	}
	return "inconsistent"
}

func short(s string) string {
	if len(s) == 64 {
		return s[:12] + "…"
	}
	return s
}

func describe(ids []IdentityCount) string {
	if len(ids) == 0 {
		return "no value at all"
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%s×%d", short(id.Value), id.N))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// Text renders the verdict: required checks first, then optional/unknown, then the one-word verdict.
func (v Verdict) Text() string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "G4A release validator — cutoff %s\n", v.Cutoff)
	fmt.Fprintf(b, "PASS is emitted only when every REQUIRED line passes; optional and unknown items never count.\n\nrequired\n")
	for _, c := range v.Required {
		fmt.Fprintf(b, "  %-5s %s — %s\n", strings.ToUpper(c.Status), c.Name, c.Detail)
	}
	if len(v.Optional) > 0 {
		fmt.Fprintf(b, "\noptional / unknown (reported, not passed)\n")
		for _, c := range v.Optional {
			fmt.Fprintf(b, "  %-10s %s — %s\n", c.Status, c.Name, c.Detail)
		}
	}
	if v.Pass {
		fmt.Fprintf(b, "\nVERDICT: PASS\n")
	} else {
		fmt.Fprintf(b, "\nVERDICT: FAIL\n")
	}
	return b.String()
}
