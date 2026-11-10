package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/db"
)

func TestAStateReadIsRecordedBeforeItsSessionOpens(t *testing.T) {
	var order []string
	read := func(_ context.Context, host, unit string) (bool, bool) {
		order = append(order, "read "+host+" "+unit)
		return false, true
	}
	rec := func(_ context.Context, host, purpose string) error {
		order = append(order, "record "+host+" "+purpose)
		return nil
	}
	if active, ok := recordedStateReads(read, rec, "independent-condition")(context.Background(), "web01", "  "); active || ok || len(order) != 0 {
		t.Fatalf("a blank unit is refused before any record is written, got active=%v ok=%v calls=%v", active, ok, order)
	}
	active, ok := recordedStateReads(read, rec, "independent-condition")(context.Background(), " web01 ", "nginx")
	if active || !ok {
		t.Fatalf("the read's own answer must pass through, got active=%v ok=%v", active, ok)
	}
	if strings.Join(order, "; ") != "record web01 independent-condition; read  web01  nginx" {
		t.Fatalf("the record must precede the session, got %v", order)
	}
	order = nil
	failing := func(context.Context, string, string) error { return errors.New("db down") }
	if active, ok := recordedStateReads(read, failing, "independent-condition")(context.Background(), "web01", "nginx"); active || ok || len(order) != 0 {
		t.Fatalf("no record, no read: got active=%v ok=%v calls=%v", active, ok, order)
	}
	if active, ok := recordedStateReads(read, nil, "independent-condition")(context.Background(), "web01", "nginx"); active || ok || len(order) != 0 {
		t.Fatalf("no recorder, no read: got active=%v ok=%v calls=%v", active, ok, order)
	}
	if recordedStateReads(nil, rec, "independent-condition") != nil {
		t.Fatal("an unwired read stays unwired")
	}
}

func TestEveryLoginUnderTGsKeyIsExplainedByItsOwnRecordOnly(t *testing.T) {
	at := func(sec, ms int) time.Time { return time.Date(2026, 11, 10, 11, 42, sec, ms*1_000_000, time.UTC) }
	readAt, execAt := at(45, 100), at(46, 521)
	login := func(ts time.Time) attribution.Evidence {
		return attribution.Evidence{Domain: "journal", Actor: "root!SHA256:tg", ActionKind: "ssh-login", ObservedAt: ts}
	}
	pair := func(execs, reads []time.Time, logins ...time.Time) []attribution.Evidence {
		times, err := explainingRecordTimes(
			sshLaneExecutionTimes(func(context.Context, string, time.Time, time.Time) ([]db.ExecutionAt, error) {
				var out []db.ExecutionAt
				for _, e := range execs {
					out = append(out, db.ExecutionAt{At: e, OpClass: "start-service"})
				}
				return out, nil
			}),
			func(context.Context, string, time.Time, time.Time) ([]time.Time, error) { return reads, nil },
		)(context.Background(), "web01", at(0, 0), at(59, 0))
		if err != nil {
			t.Fatal(err)
		}
		out := make([]attribution.Evidence, 0, len(logins))
		mine := make([]int, 0, len(logins))
		for i, l := range logins {
			out = append(out, login(l))
			mine = append(mine, i)
		}
		attribution.PairSelf(out, mine, times, 30*time.Second, 120*time.Second)
		return out
	}
	unexplained := func(ev []attribution.Evidence) int {
		n := 0
		for _, e := range ev {
			if e.SelfUnexplained {
				n++
			}
		}
		return n
	}
	if got := pair([]time.Time{execAt}, nil, at(45, 400), at(48, 0)); unexplained(got) != 1 {
		t.Fatalf("control, the 2026-11-10 regression: an execution alone explains one of the two logins, got %d unexplained", unexplained(got))
	}
	if got := pair([]time.Time{execAt}, []time.Time{readAt}, at(45, 400), at(48, 0)); unexplained(got) != 0 {
		t.Fatalf("the L2r shape: the recorded read and the execution explain both logins, got %+v", got)
	}
	if got := pair(nil, []time.Time{readAt}, at(45, 400)); unexplained(got) != 0 {
		t.Fatalf("a refused check: the recorded read explains its own login, got %+v", got)
	}
	if got := pair([]time.Time{execAt}, []time.Time{readAt}, at(45, 400), at(48, 0), at(50, 0)); unexplained(got) != 1 {
		t.Fatalf("a third login under TG's key beside one read and one execution must still read unexplained, got %d", unexplained(got))
	}
	failing := explainingRecordTimes(
		sshLaneExecutionTimes(func(context.Context, string, time.Time, time.Time) ([]db.ExecutionAt, error) { return nil, nil }),
		func(context.Context, string, time.Time, time.Time) ([]time.Time, error) {
			return nil, errors.New("state reads unreadable")
		},
	)
	if _, err := failing(context.Background(), "web01", at(0, 0), at(59, 0)); err == nil {
		t.Fatal("an unreadable state-read record must fail the read, never explain partially")
	}
}

func TestEveryStateReadWithTheActuationKeyIsRecorded(t *testing.T) {
	src := map[string]string{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var code []string
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "//") {
				code = append(code, line)
			}
		}
		src[f] = strings.Join(code, "\n")
	}
	if len(src) < 50 {
		t.Fatalf("vacuity floor: only %d source files read", len(src))
	}
	count := func(needle string) map[string]int {
		out := map[string]int{}
		for f, s := range src {
			if n := strings.Count(s, needle); n > 0 {
				out[f] = n
			}
		}
		return out
	}
	if got := count("actuationKeyStateRead("); len(got) != 1 || got["service_liveness_wiring.go"] != 2 {
		t.Fatalf("the unrecorded actuation-key read may be defined and called only inside serviceActiveReader, got %v", got)
	}
	want := map[string]int{"service_liveness_wiring.go": 1, "worker_ssh_lane.go": 1, "main.go": 1}
	got := count("sshactuation.NewNativeRunner(")
	for f, n := range got {
		if want[f] != n {
			t.Fatalf("an SSH runner over the actuation key in %s (%d): an effect runner explains its logins by its executions, a state read must go "+
				"through serviceActiveReader so it is recorded; anything else leaves TG's own logins unexplained (TG-709). all: %v", f, n, got)
		}
	}
	if got := count("serviceActiveReader("); len(got) != 3 || got["service_liveness_wiring.go"] != 1 || got["main.go"] != 1 || got["independent_condition_wire.go"] != 1 {
		t.Fatalf("serviceActiveReader is expected at its definition and two call sites, got %v", got)
	}
	if !strings.Contains(src["main.go"], `config.SecretRef(planeEnv("TG_ACTUATION_SSH_KEY", "")), stateReadRecorderFor(dbPool), "rollback-necessity")`) ||
		!strings.Contains(src["independent_condition_wire.go"], "buildIndependentCondition(cfg, stateReadRecorderFor(pool))") {
		t.Fatal("both state-read call sites must pass the durable recorder; a nil recorder refuses every read and silently disables the check")
	}
	if !strings.Contains(src["self_identity_wiring.go"], "explainingRecordTimes(sshLaneExecutionTimes(db.NewActionExecutionStore(pool).ExecutionsOn), db.NewActuationStateReadStore(pool).TimesOn)") {
		t.Fatal("the SSH-log pairing no longer reads the recorded state reads beside the executions")
	}
	for _, dir := range []string{"../../temporal", "../../core", "../../modules"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if strings.Contains(string(b), "sshactuation.NewNativeRunner(") {
				t.Errorf("%s opens an actuation SSH runner outside cmd/worker, where its reads would not be recorded", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestTheActuationKeyStateReaderRecordsBeforeItDials(t *testing.T) {
	t.Setenv("TG709_UNSET_ACTUATION_KEY", "")
	var recorded []string
	rec := func(_ context.Context, host, purpose string) error {
		recorded = append(recorded, host+" "+purpose)
		return nil
	}
	r := serviceActiveReader("/dev/null", "tg-actuator", "env:TG709_UNSET_ACTUATION_KEY", rec, "independent-condition")
	if r == nil {
		t.Fatal("a complete identity must build a reader")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if active, ok := r(ctx, "tg709.invalid", "nginx"); active || ok {
		t.Fatalf("an unresolvable key cannot establish the state, got active=%v ok=%v", active, ok)
	}
	if len(recorded) != 1 || recorded[0] != "tg709.invalid independent-condition" {
		t.Fatalf("the reader must record the read before it dials, got %v", recorded)
	}
	if serviceActiveReader("", "tg-actuator", "env:TG709_UNSET_ACTUATION_KEY", rec, "independent-condition") != nil {
		t.Fatal("an incomplete identity builds no reader")
	}
}
