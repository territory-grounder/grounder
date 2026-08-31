package db

// TG RECORDS HOW AN ALERT REACHED IT (TG-372).
//
// On 2026-08-06 I tried to answer a plain question about the running system — by what path does a LibreNMS
// alert reach TG? — and could not, from TG. The grounder publishes on 127.0.0.1:8081; the only TG listener
// exposed off-host is the console's nginx, which returns index.html for every /v1/* GET and 405 for every
// POST; the public name resolves to dc1npm01 and lands on that same nginx; no second DNS name resolves;
// no host cron forwards; nothing holds a connection to :8081; and the pull poller is off. 89 LibreNMS alerts
// arrived that day regardless.
//
// TG could not settle it because an accepted push left no trace of where it came from — the single strongest
// piece of evidence TG will ever have about its own reachability, discarded on arrival.
//
// Against a REAL Postgres deliberately: the round trip IS the mechanism. A fake would return whatever it was
// handed and would prove that the columns exist in someone's imagination.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/httpapi"
)

func deliveryFixture(ctx context.Context, t *testing.T) (*AlertLogStore, *Pool, func()) {
	t.Helper()
	dsn := skipWithoutDB(t)
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	clean := func() { _, _ = p.Exec(ctx, `DELETE FROM ingest_alert WHERE external_ref LIKE 'gold-delivery-%'`) }
	clean()
	return NewAlertLogStore(p), p, func() { clean(); p.Close() }
}

func deliveryOf(ctx context.Context, t *testing.T, p *Pool, ref string) (peer, host string) {
	t.Helper()
	if err := p.QueryRow(ctx,
		`SELECT delivery_peer, delivery_host FROM ingest_alert WHERE external_ref = $1`, ref).
		Scan(&peer, &host); err != nil {
		t.Fatalf("read back %s: %v", ref, err)
	}
	return peer, host
}

// KILLING MUTATION: drop delivery_peer/delivery_host from the INSERT column list (the state this shipped
// in). RED — the accepted alert is stored and TG still cannot say where it came from.
func TestAnAcceptedAlertRemembersHowItArrived(t *testing.T) {
	ctx := context.Background()
	st, p, done := deliveryFixture(ctx, t)
	defer done()

	rec := httpapi.AlertRecord{
		ExternalRef: "gold-delivery-1", SourceType: "librenms", ReceivedAt: time.Now().UTC(),
	}.WithDelivery("192.168.181.43:51234", "territory-grounder.example.net")
	st.Append(ctx, rec)

	peer, host := deliveryOf(ctx, t, p, "gold-delivery-1")
	if peer != "192.168.181.43:51234" {
		t.Errorf("delivery_peer = %q — the hop that handed TG the request is not recorded, which is the "+
			"question TG-372 could not answer", peer)
	}
	if host != "territory-grounder.example.net" {
		t.Errorf("delivery_host = %q — the name the caller addressed TG by is not recorded, and that is the "+
			"fact that distinguishes a direct post from one through a proxy", host)
	}
}

// EMPTY MEANS "NOT OVER HTTP", NOT "UNKNOWN". The pve-liveness poller mints envelopes in-process and calls
// the SAME RecordFromEnvelope constructor with no request behind it. That distinction is the reason the
// columns are NOT NULL DEFAULT '' rather than nullable.
//
// KILLING MUTATION: make the columns nullable and write NULL when unset. RED here — and the damage is that
// "unknown" and "in-process" would then share a representation, which is the conflation this whole family
// of tickets keeps finding.
func TestAnInProcessIntakeRecordsEmptyRatherThanNull(t *testing.T) {
	ctx := context.Background()
	st, p, done := deliveryFixture(ctx, t)
	defer done()

	// Exactly what cmd/worker's pve-liveness poller does: the constructor, no WithDelivery.
	st.Append(ctx, httpapi.AlertRecord{
		ExternalRef: "gold-delivery-inprocess", SourceType: "pve-liveness", ReceivedAt: time.Now().UTC(),
	})

	var peerNull, hostNull bool
	if err := p.QueryRow(ctx,
		`SELECT delivery_peer IS NULL, delivery_host IS NULL FROM ingest_alert WHERE external_ref = $1`,
		"gold-delivery-inprocess").Scan(&peerNull, &hostNull); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if peerNull || hostNull {
		t.Fatalf("an in-process intake wrote NULL (peer=%v host=%v) — NULL invites 'unknown' and 'did not "+
			"arrive over HTTP' to share one representation", peerNull, hostNull)
	}
	peer, host := deliveryOf(ctx, t, p, "gold-delivery-inprocess")
	if peer != "" || host != "" {
		t.Fatalf("an in-process intake invented a delivery context: peer=%q host=%q", peer, host)
	}
}

// A HOSTILE Host HEADER MUST NOT COST AN ACCEPTED ALERT ITS RECORD. The column is bounded, and WithDelivery
// truncates to the same bound — so an oversized header is recorded, clipped, rather than failing the INSERT
// and losing the alert entirely.
//
// KILLING MUTATION: remove the truncation from WithDelivery. RED — the INSERT violates the CHECK and the
// alert's front-door record is lost to a string the caller chose.
func TestAnOversizedHostHeaderIsClippedRatherThanLosingTheAlert(t *testing.T) {
	ctx := context.Background()
	st, p, done := deliveryFixture(ctx, t)
	defer done()

	rec := httpapi.AlertRecord{
		ExternalRef: "gold-delivery-hostile", SourceType: "librenms", ReceivedAt: time.Now().UTC(),
	}.WithDelivery(strings.Repeat("9", 400), strings.Repeat("a", 4000))
	st.Append(ctx, rec)

	peer, host := deliveryOf(ctx, t, p, "gold-delivery-hostile")
	if len(peer) > 100 || len(host) > 253 {
		t.Fatalf("stored past the column bound: peer=%d host=%d", len(peer), len(host))
	}
	if len(host) == 0 {
		t.Fatal("the row exists but the Host was dropped entirely — a clipped claim is evidence, an absent " +
			"one is indistinguishable from an in-process intake")
	}
}
