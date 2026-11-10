package hostdiag

// TG-662: the object-state reader. The parsers are pure; the reader drill proves it runs ONLY the allowlisted
// check-host-services argvs (byte-identical to the agent tool's steps) and reads UNOBSERVED on every path that
// cannot positively see the object — unknown host, uncovered credential, unlisted container, process-less unit.
//
// KILLING MUTATION (executed 2026-11-07): in unitReading, return Healthy:true when the unit owns no process
// (`case procs == 0` → healthy). RED: TestUnitReadingIsPositiveOnly/absent_unit (an unobservable unit read
// healthy — the fail-open the reader exists to refuse). Restored from a copy, green.

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

const (
	psFixture = `  1 Ss   init.scope            systemd
 812 Ss   nginx.service         nginx
 813 S    nginx.service         nginx
 940 T    redis.service         redis-server
 999 Sl   -                     agetty
`
	failedFixture   = "apache2.service loaded failed failed The Apache HTTP Server\n"
	inactiveFixture = "● mariadb.service loaded inactive dead MariaDB\nsshd-keygen.service loaded inactive dead OpenSSH\n"
	dockerFixture   = "mealie|running|Up 3 hours\nghostfolio|exited|Exited (1) 2 minutes ago\n"
)

func TestUnitReadingIsPositiveOnly(t *testing.T) {
	cases := []struct {
		name          string
		unit          string
		wantObserved  bool
		wantHealthy   bool
		wantDetailSub string
	}{
		{"active unit, bare name", "nginx", true, true, "active: 2 running"},
		{"active unit, typed name", "nginx.service", true, true, "active"},
		{"failed unit", "apache2", true, false, "FAILED"},
		{"inactive unit (glyph-prefixed line)", "mariadb", true, false, "inactive"},
		{"frozen unit (all processes stopped)", "redis", true, false, "FROZEN"},
		{"absent unit", "ghost", false, false, "unestablished"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := unitReading(psFixture, failedFixture, inactiveFixture, tc.unit)
			if r.Observed != tc.wantObserved || r.Healthy != tc.wantHealthy || !strings.Contains(r.Detail, tc.wantDetailSub) {
				t.Fatalf("got %+v, want observed=%v healthy=%v detail~%q", r, tc.wantObserved, tc.wantHealthy, tc.wantDetailSub)
			}
		})
	}
}

func TestContainerReadingIsPositiveOnly(t *testing.T) {
	if r := containerReading(dockerFixture, "mealie"); !r.Observed || !r.Healthy {
		t.Fatalf("a running container must read observed+healthy, got %+v", r)
	}
	if r := containerReading(dockerFixture, "ghostfolio"); !r.Observed || r.Healthy {
		t.Fatalf("an exited container must read observed+unhealthy, got %+v", r)
	}
	if r := containerReading(dockerFixture, "actualbudget"); r.Observed || r.Healthy {
		t.Fatalf("an unlisted container must read UNOBSERVED (never healthy), got %+v", r)
	}
	if r := containerReading("", "mealie"); r.Observed {
		t.Fatalf("an empty listing observes nothing, got %+v", r)
	}
}

// objRunner answers by the FULL argv (two different systemctl lists must be told apart) and records every
// argv so the drill can prove each one is a check-host-services step verbatim.
type objRunner struct {
	calls [][]string
	out   map[string]string
	fail  bool
}

func (r *objRunner) Run(_ context.Context, _ syslogng.Server, argv []string) (syslogng.RunResult, error) {
	r.calls = append(r.calls, append([]string(nil), argv...))
	if r.fail {
		return syslogng.RunResult{ExitCode: 255}, nil
	}
	return syslogng.RunResult{Stdout: []byte(r.out[strings.Join(argv, " ")]), ExitCode: 0}, nil
}

func objFixtureRunner() *objRunner {
	out := map[string]string{}
	for label, text := range map[string]string{
		svcProcStateLabel:  psFixture,
		svcFailedLabel:     failedFixture,
		svcInactiveLabel:   inactiveFixture,
		svcContainersLabel: dockerFixture,
	} {
		argv, ok := stepArgv("check-host-services", label)
		if !ok {
			panic("fixture label missing from the check-host-services steps: " + label)
		}
		out[strings.Join(argv, " ")] = text
	}
	return &objRunner{out: out}
}

func TestObjectStateReaderRunsOnlyAllowlistedStepsAndFailsClosed(t *testing.T) {
	resolver := accessResolver{accs: []Access{{Site: "nl", HostGlob: "web*", SSHUser: "tg-diag", KeyRef: config.SecretRef("env:TG_TEST_DIAG_KEY")}}}
	if NewObjectStateReader(nil, resolver) != nil || NewObjectStateReader(&objRunner{}, nil) != nil {
		t.Fatal("a reader with a missing collaborator must be nil (unwired, honestly)")
	}
	run := objFixtureRunner()
	read := NewObjectStateReader(run, resolver)

	if r := read(context.Background(), "web01", ObjectUnit, "nginx"); !r.Observed || !r.Healthy {
		t.Fatalf("active unit over the lane: %+v", r)
	}
	if r := read(context.Background(), "web01", ObjectContainer, "ghostfolio"); !r.Observed || r.Healthy {
		t.Fatalf("exited container over the lane: %+v", r)
	}
	// Every argv the reader ran is a check-host-services step, verbatim — nothing new reaches a host.
	for _, argv := range run.calls {
		found := false
		for _, c := range checks {
			if c.name != "check-host-services" {
				continue
			}
			for _, s := range c.steps {
				if strings.Join(s.argv, " ") == strings.Join(argv, " ") {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("the reader ran an argv that is not an allowlisted check step: %v", argv)
		}
	}

	// Fail-closed paths: an uncovered host, a failed read, a blank name, an unknown kind, an invalid host.
	if r := read(context.Background(), "db99", ObjectUnit, "nginx"); r.Observed {
		t.Fatalf("a host no credential rule covers must read unobserved (unmonitored), got %+v", r)
	}
	failing := NewObjectStateReader(&objRunner{fail: true}, resolver)
	if r := failing(context.Background(), "web01", ObjectUnit, "nginx"); r.Observed || r.Healthy {
		t.Fatalf("a failed read must be unobserved, got %+v", r)
	}
	if r := read(context.Background(), "web01", ObjectUnit, "  "); r.Observed {
		t.Fatalf("a blank object name must read unobserved, got %+v", r)
	}
	if r := read(context.Background(), "web01", "volume", "data"); r.Observed {
		t.Fatalf("an unknown object kind must read unobserved, got %+v", r)
	}
	if r := read(context.Background(), "web 01; rm", ObjectUnit, "nginx"); r.Observed {
		t.Fatalf("an invalid host must be refused, got %+v", r)
	}
}
