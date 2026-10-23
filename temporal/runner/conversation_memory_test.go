package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
)

// TG-80 P2-8, the seed half: a deep session whose lineage HOLDS prior turns composes the
// <conversation_memory> untrusted envelope carrying them; the same session with no reader wired (or no
// turns) composes NO such envelope — pinned byte-exactly by the goldens next door. KILLING MUTATION:
// drop the conversationCtx argument from the composeSeed call — the envelope assertion fails.
func TestConversationMemoryFoldsIntoTheDeepSeed(t *testing.T) {
	deps := testDeps()
	rec := &verbatimSeed{}
	deps.Model = rec
	turnAge := time.Now().UTC().Add(-3 * time.Hour)
	var askedKey, askedExclude string
	deps.ConversationTurns = func(_ context.Context, key, excludeRef string, limit int) ([]ConversationTurn, error) {
		askedKey, askedExclude = key, excludeRef
		if limit <= 0 {
			t.Fatal("hot-tier limit must be positive")
		}
		return []ConversationTurn{
			{ExternalRef: "TG-prior-a", Content: "outcome=no-proposal:stop — nginx was already back", CreatedAt: turnAge},
			{ExternalRef: "TG-prior-b", Content: "outcome=proposed op=restart — predicted clear in 5m", CreatedAt: turnAge.Add(-time.Hour)},
		}, nil
	}
	_, err := NewActivities(deps).InvestigateActivity(context.Background(), ingest.IncidentEnvelope{
		ExternalRef: "TG-p28-seed", Host: "web01", AlertRule: "NginxDown", Site: "dc1",
		Severity: ingest.SeverityWarning, Summary: "nginx: connection refused on :443",
	}, string(execclass.StandardAgent), ClusterMemberContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.seed, "</conversation_memory>") {
		t.Fatalf("a lineage with prior turns must compose the envelope, seed:\n%s", rec.seed[:min(len(rec.seed), 800)])
	}
	if !strings.Contains(rec.seed, "TG-prior-a") || !strings.Contains(rec.seed, "nginx was already back") {
		t.Fatal("the prior turns' digests must ride the block")
	}
	if askedKey != "nginxdown|web01" && !strings.Contains(askedKey, "|web01") {
		t.Fatalf("the lineage key must be canonical-rule|host, got %q", askedKey)
	}
	if askedExclude != "TG-p28-seed" {
		t.Fatalf("the asking session must exclude itself, got %q", askedExclude)
	}
}

// No reader wired (the pre-feature deployment shape): NO envelope, and the read is never consulted.
func TestNoConversationReaderMeansNoBlock(t *testing.T) {
	deps := testDeps()
	rec := &verbatimSeed{}
	deps.Model = rec
	_, err := NewActivities(deps).InvestigateActivity(context.Background(), ingest.IncidentEnvelope{
		ExternalRef: "TG-p28-none", Host: "web01", AlertRule: "NginxDown", Site: "dc1",
		Severity: ingest.SeverityWarning, Summary: "nginx down",
	}, string(execclass.StandardAgent), ClusterMemberContext{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.seed, "conversation_memory>") && strings.Contains(rec.seed, "</conversation_memory>") {
		t.Fatal("no reader wired must compose no conversation envelope")
	}
}

// The terminal half: RecordTriageActivity folds the session's digest onto its lineage — best-effort,
// keyed like the read, carrying the outcome and the screened claim. KILLING MUTATION: drop the
// ConversationAppend call — the capture assertions fail.
func TestTerminalRecorderAppendsTheLineageDigest(t *testing.T) {
	deps := testDeps()
	recorded := map[string]string{}
	deps.TriageRecord = func(context.Context, judge.TriageRow) error { return nil }
	var gotKey, gotRef, gotContent string
	deps.ConversationAppend = func(_ context.Context, key, ref, content string) error {
		gotKey, gotRef, gotContent = key, ref, content
		return nil
	}
	_ = recorded
	res, err := NewActivities(deps).RecordTriageActivity(context.Background(), judge.TriageRow{
		ExternalRef: "TG-p28-term", Host: "web01", AlertRule: "NginxDown",
		Outcome: "no-proposal:stop", StopReason: "grounded-stop",
		Conclusion: "nginx recovered on its own; the alert lagged the recovery",
	})
	if err != nil || !res.Recorded {
		t.Fatalf("record: %v %+v", err, res)
	}
	if !strings.HasSuffix(gotKey, "|web01") || gotRef != "TG-p28-term" {
		t.Fatalf("digest keyed wrong: key=%q ref=%q", gotKey, gotRef)
	}
	if !strings.Contains(gotContent, "outcome=no-proposal:stop") || !strings.Contains(gotContent, "recovered on its own") {
		t.Fatalf("the digest must carry outcome and claim, got %q", gotContent)
	}
}

// The pure pieces: key derivation is strict about degenerate halves; the digest bounds itself; the
// rendered block neutralization-survives a hostile turn (the envelope defenses run downstream, but the
// renderer itself must not invent trust).
func TestConversationPureHelpers(t *testing.T) {
	if conversationKey("", "web01") != "" || conversationKey("NginxDown", " ") != "" {
		t.Fatal("a degenerate half must yield no lineage")
	}
	long := strings.Repeat("x", 2000)
	d := conversationDigest(judge.TriageRow{Outcome: "stop", Conclusion: long})
	if len([]rune(d)) > conversationDigestRunes+1 {
		t.Fatalf("digest unbounded: %d runes", len([]rune(d)))
	}
	if conversationMemoryContext(nil, time.Now()) != "" {
		t.Fatal("no turns must render no block")
	}
}
