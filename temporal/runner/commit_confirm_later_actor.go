package runner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/db"
)

type laterActorReading struct {
	moved  bool
	clear  bool
	detail string
}

func (a *Activities) laterActorOn(ctx context.Context, row db.CommitConfirmRow, target string) laterActorReading {
	ev, have := a.causalityEvidence(ctx, CausalityQuery{
		Host: target, TargetHost: target, AlertRule: row.AlertRule, OpClass: row.OpClass,
		IncidentAt: row.ArmedAt, ExecutedAt: row.ArmedAt, ClearedAt: time.Now(),
	})
	return readLaterActor(target, row.ArmedAt, have, ev)
}

func readLaterActor(target string, armedAt time.Time, have bool, ev CausalityEvidence) laterActorReading {
	since := armedAt.UTC().Format(time.RFC3339)
	if !have || !ev.ActorsKnown || !ev.ActorCovered || ev.ActorUnattributed > 0 {
		return laterActorReading{detail: fmt.Sprintf(
			"the actor evidence for %s since the window armed at %s cannot show who moved it (read=%v known=%v covered=%v unattributed=%d; %s)",
			target, since, have, ev.ActorsKnown, ev.ActorCovered, ev.ActorUnattributed, strings.Join(ev.Notes, "; "))}
	}
	switch ev.ActorTaxonomy {
	case attribution.AttributedAuthorized.String(), attribution.AuthorizedTest.String():
		return laterActorReading{moved: true, detail: fmt.Sprintf(
			"superseded: an authorized actor (%s, %d record(s)) acted on %s after this window armed at %s, so its inverse would act on top of that actor's change",
			ev.ActorTaxonomy, ev.ActorActions, target, since)}
	case attribution.AttributedSuspicious.String():
		return laterActorReading{detail: fmt.Sprintf(
			"an unsanctioned actor (%d record(s)) acted on %s since the window armed at %s", ev.ActorActions, target, since)}
	case attribution.AttributedSelf.String(), "":
		return laterActorReading{clear: true}
	}
	return laterActorReading{detail: fmt.Sprintf("unrecognised actor reading %q for %s", ev.ActorTaxonomy, target)}
}

func stateWord(running bool) string {
	if running {
		return "running"
	}
	return "not running"
}
