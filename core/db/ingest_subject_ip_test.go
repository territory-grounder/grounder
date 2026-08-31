package db

// THE INCIDENT'S SUBJECT REACHES THE RECORD (TG-373).
//
// IncidentEnvelope.IP was written by FOUR ingest modules and read by nothing:
//
//	modules/ingest/crowdsec:216       raw2.IP = ipVal
//	modules/ingest/prometheus-alertmanager:156  raw.IP = target
//	modules/ingest/librenms/normalize.go:148    raw.IP = p.Host
//	modules/ingest/authlog:244        raw.IP = ip
//	core/ingest/normalize.go:36       ip, err := validateIP(raw.IP)
//
// The complete non-test list. Four writers, one validator, no consumer — declared-but-dead on the ingest
// spine, the shape TG-66 deleted from the agent-step spine and built a floor against there.
//
// Measured 2026-08-06: 48 of 165 prometheus-alertmanager rows have no host, and 40 of those carry an
// `instance` label. The module is CORRECT — hostFromInstance("10.0.2.193:8080") -> "10.0.2.193", which
// net.ParseIP accepts, so it goes to raw.IP and Host is properly left empty. The subject was extracted,
// parsed, validated, and dropped one layer later. Among those 40 are the three alerts TG received about its
// own AWX outage, each of which minted a triage session that could not say what it was about.

import (
	"context"
	"net"
	"testing"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/httpapi"
)

func subjectFixture(ctx context.Context, t *testing.T) (*AlertLogStore, *Pool, func()) {
	t.Helper()
	dsn := skipWithoutDB(t)
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	clean := func() { _, _ = p.Exec(ctx, `DELETE FROM ingest_alert WHERE external_ref LIKE 'gold-subject-%'`) }
	clean()
	return NewAlertLogStore(p), p, func() { clean(); p.Close() }
}

// KILLING MUTATION: drop SubjectIP from RecordFromEnvelope, or subject_ip from the INSERT (the state this
// shipped in). RED — the incident is stored with no subject at all, which is what made TG's own AWX outage
// unattributable.
func TestAnIPIdentifiedIncidentKeepsItsSubject(t *testing.T) {
	ctx := context.Background()
	st, p, done := subjectFixture(ctx, t)
	defer done()

	// Exactly the shape prometheus-alertmanager produces for a kube-state-metrics alert: no Host, an IP.
	env := coreingest.IncidentEnvelope{
		ExternalRef: "gold-subject-ip", AlertRule: "KubePodCrashLooping",
		IP: net.ParseIP("10.0.2.193"), ReceivedAt: time.Now().UTC(),
	}
	st.Append(ctx, httpapi.RecordFromEnvelope("prometheus-alertmanager", env, "wf-1"))

	var ip, host *string
	if err := p.QueryRow(ctx,
		`SELECT host(subject_ip), nullif(host,'') FROM ingest_alert WHERE external_ref = $1`,
		"gold-subject-ip").Scan(&ip, &host); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if ip == nil {
		t.Fatal("subject_ip is NULL for an incident whose ONLY identifier was its address — the envelope " +
			"carried it, core validated it, and the record dropped it. That is the state in which TG " +
			"triaged its own AWX outage without being able to say what it was about")
	}
	if *ip != "10.0.2.193" {
		t.Errorf("subject_ip = %q, want 10.0.2.193", *ip)
	}
	if host != nil {
		t.Errorf("host = %q — an IP-identified subject must NOT be written into the host column; the estate "+
			"graph resolves by name and a bare address there would look like a resolvable host and never match", *host)
	}
}

// A NAME-IDENTIFIED INCIDENT WRITES NULL, NOT AN EMPTY LITERAL. subject_ip is inet: "" is not an address,
// and NULL is the honest value for "this subject was named". Note the deliberate contrast with migration
// 0062's delivery_peer/delivery_host, which are NOT NULL DEFAULT '' because there '' MEANS something
// ("did not arrive over HTTP"). Same table, opposite call, for a stated reason.
//
// KILLING MUTATION: pass rec.SubjectIP straight through instead of nullIfEmpty. RED — the INSERT fails the
// inet cast and the alert's whole front-door record is lost, not merely its subject.
func TestANameIdentifiedIncidentStoresNullNotAnEmptyAddress(t *testing.T) {
	ctx := context.Background()
	st, p, done := subjectFixture(ctx, t)
	defer done()

	env := coreingest.IncidentEnvelope{
		ExternalRef: "gold-subject-named", AlertRule: "Device-rebooted",
		Host: "dc1pve01", ReceivedAt: time.Now().UTC(),
	}
	st.Append(ctx, httpapi.RecordFromEnvelope("librenms", env, "wf-2"))

	var isNull bool
	var host string
	if err := p.QueryRow(ctx,
		`SELECT subject_ip IS NULL, host FROM ingest_alert WHERE external_ref = $1`,
		"gold-subject-named").Scan(&isNull, &host); err != nil {
		t.Fatalf("read back — if this is a scan/insert error the empty string reached an inet column: %v", err)
	}
	if !isNull {
		t.Error("a name-identified incident stored a non-NULL subject_ip")
	}
	if host != "dc1pve01" {
		t.Errorf("host = %q, want dc1pve01 — the named path must be untouched", host)
	}
}

// A nil envelope IP must not render as the string "<nil>". net.IP(nil).String() returns exactly that, and it
// would pass every non-empty check while meaning the opposite of what it says.
//
// KILLING MUTATION: replace ipString with env.IP.String(). RED.
func TestANilEnvelopeIPDoesNotBecomeTheStringNil(t *testing.T) {
	rec := httpapi.RecordFromEnvelope("librenms", coreingest.IncidentEnvelope{
		ExternalRef: "x", Host: "h", ReceivedAt: time.Now().UTC(),
	}, "")
	if rec.SubjectIP != "" {
		t.Fatalf("SubjectIP = %q for an envelope with no address — want empty. net.IP(nil).String() is "+
			"\"<nil>\", which would store four characters that read as an address and are not one", rec.SubjectIP)
	}
}
