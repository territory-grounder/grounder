package alertmanager

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func tg708Alert(alertname, site, startsAt string) string {
	labels := fmt.Sprintf(`"alertname":%q,"instance":"app01:9100","severity":"warning"`, alertname)
	if site != "" {
		labels += fmt.Sprintf(`,"site":%q`, site)
	}
	return fmt.Sprintf(`{"status":"firing","labels":{%s},"annotations":{"summary":"application down"},"startsAt":%q}`, labels, startsAt)
}

func tg708Webhook(truncated int, alerts ...string) []byte {
	return []byte(fmt.Sprintf(`{"version":"4","status":"firing","truncatedAlerts":%d,"alerts":[%s]}`, truncated, strings.Join(alerts, ",")))
}

func TestEveryAlertInAWebhookIsAdmittedSkippedOrRejectedWithItsReason(t *testing.T) {
	valid := fixedNow().Add(-time.Minute).Format(time.RFC3339)
	tooOld := fixedNow().Add(-31 * 24 * time.Hour).Format(time.RFC3339)
	cases := []struct {
		name              string
		alerts            []string
		admitted, skipped int
		rejectedAt        []int
		reason            string
		wantSite          string
		checkSite         bool
	}{
		{"a valid alert with its site", []string{tg708Alert("AppDown", "dc1", valid)}, 1, 0, nil, "", "dc1", true},
		{"a valid alert with no site is admitted, site empty", []string{tg708Alert("AppDown", "", valid)}, 1, 0, nil, "", "", true},
		{"the L3 run-1 payload: no site and a strftime pattern for startsAt", []string{tg708Alert("AppDown", "", "%Y-%m-%dT%H:%M:%SZ")}, 0, 0, []int{0}, "malformed startsAt", "", false},
		{"a startsAt older than the 30-day bound", []string{tg708Alert("AppDown", "dc1", tooOld)}, 0, 0, []int{0}, "older than", "", false},
		{"a severity outside the vocabulary", []string{strings.Replace(tg708Alert("AppDown", "dc1", valid), `"severity":"warning"`, `"severity":"sev2"`, 1)}, 0, 0, []int{0}, "severity", "", false},
		{"a mixed webhook keeps the good alert and names the bad one", []string{
			tg708Alert("AppDown", "dc1", valid),
			tg708Alert("AppDown2", "dc1", "not-a-time"),
			`{"status":"firing","labels":{"alertname":"Watchdog","severity":"none"},"startsAt":"` + valid + `"}`,
		}, 1, 1, []int{1}, "malformed startsAt", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := mod().NormalizeBatch(context.Background(), tg708Webhook(0, tc.alerts...))
			if err != nil {
				t.Fatalf("a well-formed webhook must normalize: %v", err)
			}
			if len(b.Envelopes) != tc.admitted || b.Skipped != tc.skipped || len(b.Rejected) != len(tc.rejectedAt) {
				t.Fatalf("admitted %d skipped %d rejected %d, want %d %d %d: %+v", len(b.Envelopes), b.Skipped, len(b.Rejected), tc.admitted, tc.skipped, len(tc.rejectedAt), b)
			}
			if b.Items() != len(tc.alerts) {
				t.Fatalf("every alert must be accounted for: %d of %d", b.Items(), len(tc.alerts))
			}
			for i, at := range tc.rejectedAt {
				if b.Rejected[i].Index != at || !strings.Contains(b.Rejected[i].Reason, tc.reason) {
					t.Fatalf("rejection %d = %+v, want index %d with a reason naming %q", i, b.Rejected[i], at, tc.reason)
				}
			}
			if tc.checkSite && b.Envelopes[0].Site != tc.wantSite {
				t.Fatalf("site %q, want %q", b.Envelopes[0].Site, tc.wantSite)
			}
		})
	}
}

func TestTheSendersTruncationCountIsCarried(t *testing.T) {
	valid := fixedNow().Add(-time.Minute).Format(time.RFC3339)
	b, err := mod().NormalizeBatch(context.Background(), tg708Webhook(7, tg708Alert("AppDown", "dc1", valid)))
	if err != nil {
		t.Fatal(err)
	}
	if b.Truncated != 7 || len(b.Envelopes) != 1 {
		t.Fatalf("truncatedAlerts must be carried, got %+v", b)
	}
	b, err = mod().NormalizeBatch(context.Background(), tg708Webhook(-3, tg708Alert("AppDown", "dc1", valid)))
	if err != nil {
		t.Fatal(err)
	}
	if b.Truncated != 0 {
		t.Fatalf("a negative truncation count is not a count, got %d", b.Truncated)
	}
	odd := []byte(fmt.Sprintf(`{"version":"4","status":"firing","truncatedAlerts":"lots","alerts":[%s]}`, tg708Alert("AppDown", "dc1", valid)))
	b, err = mod().NormalizeBatch(context.Background(), odd)
	if err != nil || b.Truncated != 0 || len(b.Envelopes) != 1 {
		t.Fatalf("a malformed truncatedAlerts must not cost the webhook its alerts: err=%v %+v", err, b)
	}
}

func TestTheSingleAlertPathNamesTheGrammarFailure(t *testing.T) {
	_, err := mod().Normalize(context.Background(), tg708Webhook(0, tg708Alert("AppDown", "", "%Y-%m-%dT%H:%M:%SZ")))
	if err == nil || !strings.Contains(err.Error(), "malformed startsAt") {
		t.Fatalf("the single-alert path must say which field failed, got %v", err)
	}
}
