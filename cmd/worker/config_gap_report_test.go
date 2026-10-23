package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/audit"
)

var errTestAppend = errors.New("ledger unavailable")

// A CONFIG GAP MUST REACH A SURFACE AN OPERATOR OPENS.
//
// All three boot checks reported only via log.Printf, and no HTTP surface carries worker stdout — verified
// against core/httpapi/router.go, which serves /v1/events and /v1/ledger, and the console's Logs·Evidence
// view, which is fed by the ledger and ingest alerts. So findings that silently withdraw autonomy existed
// only in `docker logs` on the host. This folds them into one governance-ledger reason.
func TestConfigGapReportNamesEveryFindingItIsGiven(t *testing.T) {
	reason, any := configGapReport(
		[]string{"dc1mealie01", "dc1excalidraw01"},
		[]attribution.DomainConfigGap{
			{Domain: "journal", NoSelfActor: true, NoSanctioned: true},
			{Domain: "netbox", NoSelfActor: true},
			{Domain: "awx", NoSanctioned: true},
		},
		"TG_PVE_INSECURE is TRUE (…) while TG_PROXMOX_INSECURE is FALSE/UNSET (…)",
		nil,
	)
	if !any {
		t.Fatal("three findings were supplied and the report said there was nothing to say")
	}
	for _, want := range []string{
		"dc1mealie01", "dc1excalidraw01", // the uncovered guests, by name
		`"journal"`, `"netbox"`, `"awx"`, // every armed domain with a gap
		"INCLUDING TG's own actions", // the both-missing case is the most severe and must be distinguished
		"TG's OWN actions",           // self-actor-only case
		"every non-TG actor",         // sanctioned-only case
		"Proxmox TLS flags disagree", // the TLS finding
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("the ledger reason omits %q — an operator reading the ledger cannot act on a finding "+
				"that is not in it.\nGot: %s", want, reason)
		}
	}
}

// A CLEAN CONFIG WRITES NOTHING. The worker restarts on every deploy and the governance ledger is append-only
// and hash-chained — it cannot be pruned. An unconditional append would grow the audit spine with noise and
// train readers to skip config rows.
func TestACleanConfigProducesNoLedgerEntry(t *testing.T) {
	if reason, any := configGapReport(nil, nil, "", nil); any || reason != "" {
		t.Errorf("a fully-configured system produced a ledger entry (any=%v reason=%q) — an append-only "+
			"ledger must not accrue a row per boot for a system with nothing wrong", any, reason)
	}
	// Empty-but-non-nil inputs are the realistic shape (the helpers return empty slices, not nil).
	if _, any := configGapReport([]string{}, []attribution.DomainConfigGap{}, "", nil); any {
		t.Error("empty slices were treated as findings")
	}
}

// Each finding must be independently sufficient: any ONE of them alone produces an entry. Requiring two would
// silently drop a lone gap, which is the common case.
func TestAnySingleFindingIsEnoughToReport(t *testing.T) {
	cases := []struct {
		name  string
		hosts []string
		gaps  []attribution.DomainConfigGap
		tls   string
	}{
		{name: "only an uncovered guest", hosts: []string{"dc1mealie01"}},
		{name: "only a domain gap", gaps: []attribution.DomainConfigGap{{Domain: "journal", NoSelfActor: true}}},
		{name: "only a TLS disagreement", tls: "TG_PVE_INSECURE is TRUE while TG_PROXMOX_INSECURE is FALSE/UNSET"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, any := configGapReport(c.hosts, c.gaps, c.tls, nil)
			if !any {
				t.Errorf("%s did not produce a ledger entry on its own — a lone gap is the common case, not "+
					"the exception", c.name)
			}
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s produced any=true with an EMPTY reason — a ledger row with no content is worse "+
					"than none, because it looks like the finding was recorded", c.name)
			}
		})
	}
}

// A domain carrying BOTH absences must not be reported as if it carried only one. That case is strictly worse
// — it includes TG's own identity — and collapsing it would understate a security-escalation-on-self.
func TestTheBothMissingCaseIsNotCollapsedIntoEither(t *testing.T) {
	both, _ := configGapReport(nil, []attribution.DomainConfigGap{{Domain: "journal", NoSelfActor: true, NoSanctioned: true}}, "", nil)
	selfOnly, _ := configGapReport(nil, []attribution.DomainConfigGap{{Domain: "journal", NoSelfActor: true}}, "", nil)
	sancOnly, _ := configGapReport(nil, []attribution.DomainConfigGap{{Domain: "journal", NoSanctioned: true}}, "", nil)

	if both == selfOnly || both == sancOnly {
		t.Errorf("the both-missing case renders identically to a single-absence case, so the more severe "+
			"condition is indistinguishable in the ledger.\nboth:     %s\nselfOnly: %s\nsancOnly: %s",
			both, selfOnly, sancOnly)
	}
	if selfOnly == sancOnly {
		t.Errorf("the two single-absence cases render identically, so an operator cannot tell whether TG's "+
			"OWN identity is the one missing.\n%s", selfOnly)
	}
}

// countingAppender records every append so the GUARD can be asserted, not just the message it would carry.
type countingAppender struct {
	calls []audit.GovDecision
	err   error
}

func (c *countingAppender) Append(d audit.GovDecision) (audit.LedgerEntry, error) {
	c.calls = append(c.calls, d)
	return audit.LedgerEntry{Seq: int64(len(c.calls))}, c.err
}

// THE APPEND GUARD ITSELF, not the message it would build.
//
// ★ THIS TEST EXISTS BECAUSE A MUTATION CONTROL FAILED TO GO RED. Removing the `if any` guard so the worker
// appended on EVERY boot left the whole suite green: every oracle covered configGapReport, the pure
// assembler, and nothing covered the caller that decides whether to write at all. The rule "an append-only
// ledger must not accrue a row per boot" lived in untested wiring.
func TestTheLedgerIsWrittenOnlyWhenThereIsAGap(t *testing.T) {
	for _, c := range []struct {
		name  string
		hosts []string
		gaps  []attribution.DomainConfigGap
		tls   string
		want  int
	}{
		{name: "clean config writes nothing", want: 0},
		{name: "empty slices write nothing", hosts: []string{}, gaps: []attribution.DomainConfigGap{}, want: 0},
		{name: "one uncovered guest writes once", hosts: []string{"dc1mealie01"}, want: 1},
		{name: "three findings still write ONCE", hosts: []string{"dc1mealie01"},
			gaps: []attribution.DomainConfigGap{{Domain: "journal", NoSelfActor: true}}, tls: "flags disagree", want: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := &countingAppender{}
			if err := appendConfigGapReport(a, c.hosts, c.gaps, c.tls, nil); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(a.calls) != c.want {
				t.Errorf("appended %d row(s), want %d — the worker restarts on every deploy and the ledger "+
					"is append-only, so an extra row here is permanent", len(a.calls), c.want)
			}
			if c.want == 1 {
				if a.calls[0].Decision != "config:gap-at-boot" {
					t.Errorf("decision = %q, want config:gap-at-boot (the label an operator filters on)", a.calls[0].Decision)
				}
				if a.calls[0].Withheld {
					t.Error("Withheld is TRUE — that flag means autonomy was withheld for a DECISION and feeds " +
						"the withheld-rate metrics; a boot-time observation must not inflate a governance number")
				}
				if a.calls[0].Reason == "" {
					t.Error("an empty reason was appended — a ledger row with no content looks like the finding was recorded")
				}
			}
		})
	}
}

// An append failure must be REPORTED, not swallowed, and must never be turned into a boot failure. A
// diagnostic that can stop the control plane is a worse defect than the gap it reports.
func TestAnAppendFailureIsReturnedAndNeverFatal(t *testing.T) {
	a := &countingAppender{err: errTestAppend}
	err := appendConfigGapReport(a, []string{"dc1mealie01"}, nil, "", nil)
	if err == nil {
		t.Error("a failing ledger swallowed its error — the caller logs this, and a silent failure means the " +
			"finding reached NO surface at all, which is the defect this whole change removes")
	}
	// A nil ledger is a no-op, not a panic: the worker runs without a durable ledger in some configurations.
	if err := appendConfigGapReport(nil, []string{"dc1mealie01"}, nil, "", nil); err != nil {
		t.Errorf("a nil ledger returned an error: %v", err)
	}
}

// A DIAGNOSTIC MUST NAME A REMEDY THAT EXISTS.
//
// The first version of this report told an operator that the fix was "declaring the identity for each domain
// they arm". There is no such path and there deliberately never will be: ParseConfig does not read
// SelfActors from the ruleset, because a self-identity an operator can type is one an attacker can be named
// in. It is derived from the domain's CREDENTIAL at the composition root, and written in exactly one place in
// the tree — for "pve".
//
// So the message sent whoever read it to edit a config file that cannot affect the condition. A warning whose
// remedy does not exist is worse than silence: it converts a real finding into wasted effort and then into
// distrust of the next warning.
func TestTheSelfActorGapNamesARemedyThatExists(t *testing.T) {
	reason, any := configGapReport(nil,
		[]attribution.DomainConfigGap{{Domain: "journal", NoSelfActor: true}}, "", nil)
	if !any {
		t.Fatal("no report produced for a self-actor gap")
	}
	// It must point at the credential/composition root, and say that only pve is wired — the fact that makes
	// the finding actionable rather than mysterious.
	for _, want := range []string{"credential", "pve"} {
		if !strings.Contains(strings.ToLower(reason), want) {
			t.Errorf("the self-actor gap message does not mention %q, so a reader cannot tell WHERE the "+
				"identity comes from.\nGot: %s", want, reason)
		}
	}
	// It must NOT imply the ruleset is the fix for a self-actor.
	if strings.Contains(reason, "declaring the identity") {
		t.Error("the message still tells an operator to DECLARE the identity — SelfActors is not parsed from " +
			"the ruleset, so that instruction sends them to edit a file that cannot affect this")
	}

	// The SANCTIONED half is the opposite: it IS ruleset-declared, and the message must not conflate them.
	sanc, _ := configGapReport(nil,
		[]attribution.DomainConfigGap{{Domain: "journal", NoSanctioned: true}}, "", nil)
	if strings.Contains(strings.ToLower(sanc), "credential") {
		t.Errorf("the sanctioned-principals gap points at the credential engine — sanctioned principals ARE "+
			"declared in the ruleset, and conflating the two remedies makes both useless.\nGot: %s", sanc)
	}
}
