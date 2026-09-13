package main

// A SELF-DIRECTED HEALTHCHECK, because the image has no shell (TG-170).
//
// compose needs a runtime HEALTHCHECK so a wedged process is detected rather than sitting "Up" forever.
// The usual form — CMD-SHELL with curl or wget — cannot work here: grounder and worker ship as distroless
// images with no shell and no HTTP client. Measured 2026-08-05:
//
//	docker exec territory-grounder-grounder-1 /bin/sh -c ...
//	  -> exec: "/bin/sh": stat /bin/sh: no such file or directory
//
// Adding a shell or curl to the runtime image to satisfy a healthcheck would widen the attack surface of
// every deployed container to gain a liveness probe, which trades away more than it buys — the image is
// distroless deliberately (spec/009 REQ-903).
//
// So the binary probes itself. `/app -healthcheck` performs one local HTTP GET against this process's own
// listener and exits 0 on a 2xx, non-zero otherwise. It needs nothing that is not already in the image.
//
// IT PROBES THE LISTENER, NOT THE PROCESS. `docker inspect` already tells you the process is running; that
// is exactly the state a wedged server is in. The only useful question is whether it still answers, so the
// check is an HTTP round-trip and a deliberately short timeout — a probe that waits 30s cannot distinguish
// "slow" from "hung" within the healthcheck interval.

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// healthcheckTimeout is short on purpose: the compose healthcheck interval is 30s, so a probe that can
// block longer than that would overlap itself and report the previous result.
const healthcheckTimeout = 3 * time.Second

// probeSelf GETs path on the given listen address and returns nil when it answers 2xx.
//
// A listen address of ":8080" or "0.0.0.0:8080" is dialled on the loopback: the probe runs INSIDE the
// container, and a wildcard bind is not a dialable host.
func probeSelf(listenAddr, path string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil {
		return fmt.Errorf("healthcheck: unparseable listen address %q: %w", listenAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + path

	c := &http.Client{Timeout: healthcheckTimeout}
	resp, err := c.Get(url)
	if err != nil {
		return fmt.Errorf("healthcheck: %s unreachable: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("healthcheck: %s returned %d", url, resp.StatusCode)
	}
	return nil
}

// runHealthcheck is the -healthcheck entry point. It never returns.
func runHealthcheck(listenAddr, path string) {
	if err := probeSelf(listenAddr, path); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}
