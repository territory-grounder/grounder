package acceptance

import (
	"context"
	"fmt"
	"strings"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
)

// The syslog-ng investigation connector (REQ-823) binds its own scenarios here via init() — no edit to
// the shared harness. It drives the REAL tools through an injected fake runner (CI has no SSH): a
// happy-path read that must route to the correct site server as a fixed argv, and a path-traversal host
// that must be refused before any read.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerSyslogngSteps)
}

// sgFakeRunner records the routed server + remote argv and returns canned ASA log text — proving the tool
// path end to end with no live SSH.
type sgFakeRunner struct {
	gotServer syslogng.Server
	gotArgv   []string
	calls     int
}

func (f *sgFakeRunner) Run(_ context.Context, s syslogng.Server, argv []string) (syslogng.RunResult, error) {
	f.gotServer = s
	f.gotArgv = append([]string(nil), argv...)
	f.calls++
	return syslogng.RunResult{ExitCode: 0, Stdout: []byte(
		"Jul 15 12:00:02 dc1fw01 %ASA-6-302014: Teardown TCP connection 1 for outside:192.0.2.9/443 duration 0:00:01 bytes 12\n")}, nil
}

type syslogngWorld struct {
	tools  []agent.Tool
	runner *sgFakeRunner
	res    agent.ToolResult
}

func (w *syslogngWorld) get(name string) agent.Tool {
	for _, t := range w.tools {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

func registerSyslogngSteps(sc *godog.ScenarioContext) {
	w := &syslogngWorld{}

	sc.Step(`^the syslog-ng investigation connector is configured with an NL server and a GR server and an injected runner$`, func() error {
		servers := syslogng.ParseServers("NL|dc1syslogng01|root|env:SYSLOGNG_SSH_KEY|/mnt/logs/syslog-ng;GR|dc2syslogng01|root|env:SYSLOGNG_SSH_KEY|/mnt/logs/syslog-ng")
		if len(servers) != 2 {
			return fmt.Errorf("expected 2 configured servers, got %d", len(servers))
		}
		w.runner = &sgFakeRunner{}
		w.tools = syslogng.NewTools(servers, w.runner)
		if w.get("get-host-logs") == nil || w.get("search-host-logs") == nil {
			return fmt.Errorf("both investigation tools must be built")
		}
		for _, t := range w.tools {
			if !t.ReadOnly() {
				return fmt.Errorf("tool %s must be read-only", t.Name())
			}
		}
		return nil
	})

	sc.Step(`^the agent requests the recent logs of an NL device host$`, func() error {
		res, err := w.get("get-host-logs").Invoke(context.Background(), map[string]string{"host": "dc1fw01"})
		if err != nil {
			return fmt.Errorf("a read-only tool must not return a Go error: %w", err)
		}
		w.res = res
		return nil
	})

	sc.Step(`^the read is routed to the NL server as a fixed tail argv and the ASA log lines are returned as a read-only observation$`, func() error {
		if !w.res.Success {
			return fmt.Errorf("expected a successful observation, got %q", w.res.Output)
		}
		if w.runner.gotServer.SSHHost != "dc1syslogng01" {
			return fmt.Errorf("must route to the NL syslog server, routed to %q", w.runner.gotServer.SSHHost)
		}
		wantArgv := []string{"tail", "-n", "200", "--", "/mnt/logs/syslog-ng/dc1fw01/today.log"}
		if strings.Join(w.runner.gotArgv, " ") != strings.Join(wantArgv, " ") {
			return fmt.Errorf("read must be a fixed tail argv, got %v", w.runner.gotArgv)
		}
		if !strings.Contains(w.res.Output, "%ASA-6-302014") {
			return fmt.Errorf("the ASA log lines must be surfaced, got %q", w.res.Output)
		}
		return nil
	})

	sc.Step(`^the agent requests logs for a host carrying a path-traversal sequence$`, func() error {
		res, err := w.get("get-host-logs").Invoke(context.Background(), map[string]string{"host": "../../etc/passwd"})
		if err != nil {
			return fmt.Errorf("a refused read must not return a Go error: %w", err)
		}
		w.res = res
		return nil
	})

	sc.Step(`^the connector refuses without leaking a path and the injected runner is never called$`, func() error {
		if w.res.Success {
			return fmt.Errorf("a traversal host must be refused, got success: %q", w.res.Output)
		}
		if !strings.HasPrefix(w.res.Output, "refused:") {
			return fmt.Errorf("the refusal must be explicit, got %q", w.res.Output)
		}
		if strings.Contains(w.res.Output, "/mnt/") {
			return fmt.Errorf("a refusal must not leak a filesystem path: %q", w.res.Output)
		}
		if w.runner.calls != 0 {
			return fmt.Errorf("a refused host must never reach the runner (calls=%d)", w.runner.calls)
		}
		return nil
	})
}
