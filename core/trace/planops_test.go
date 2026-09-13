package trace

import "testing"

func TestClassifyPlanOp(t *testing.T) {
	cases := []struct{ opClass, op, want string }{
		{"restart-service", "restart", "change"}, // restart must NOT resolve to the 'start' add-token
		{"reload-config", "", "change"},
		{"kubectl-get", "get", ""},   // read-only → no plan op
		{"get-device-status", "", ""},
		{"", "list", ""},
		{"delete-pod", "delete", "destroy"},
		{"drain", "", "change"},           // cordon/drain are changes, not destroys
		{"evict-pods", "evict", "destroy"},
		{"create-cronjob", "create", "add"},
		{"deploy", "", "add"},
		{"", "start", "add"},
		{"frobnicate", "", "change"}, // unknown mutating op → honest "change", never fabricated add/destroy
		{"", "", ""},                 // nothing → no op
		// whole-token matching: a mutating class whose NAME merely CONTAINS a read token must NOT be mis-read as
		// read-only (the substring bug — a tracer must never under-report a mutation).
		{"healthcheck-restart", "", "change"}, // contains "check" but restart(change) wins
		{"set-status-page", "", "change"},     // contains "status" but set(change) wins
		{"ping-flood-stop", "", "destroy"},    // contains "ping" but stop(destroy) wins
	}
	for _, c := range cases {
		if got := ClassifyPlanOp(c.opClass, c.op); got != c.want {
			t.Errorf("ClassifyPlanOp(%q,%q) = %q, want %q", c.opClass, c.op, got, c.want)
		}
	}
}

func TestProjectPlanOps(t *testing.T) {
	// a read-only op yields no plan op (mutates no end-state).
	if ops := ProjectPlanOps("host1", "kubectl-get", "get", nil); ops != nil {
		t.Errorf("read-only op produced plan ops: %+v", ops)
	}
	// a change op yields exactly one op with a formatted, param-sorted target.
	ops := ProjectPlanOps("dc1k8s-w3", "restart-service", "restart", map[string]string{"unit": "nginx", "after": "5s"})
	if len(ops) != 1 {
		t.Fatalf("want 1 plan op, got %d: %+v", len(ops), ops)
	}
	if ops[0].Op != "change" {
		t.Errorf("op polarity = %q, want change", ops[0].Op)
	}
	// params must be sorted deterministically (after before unit).
	want := "dc1k8s-w3 — restart-service (restart): after=5s unit=nginx"
	if ops[0].T != want {
		t.Errorf("target = %q, want %q", ops[0].T, want)
	}
	// op equal to op_class is not repeated in parentheses.
	if o := ProjectPlanOps("h", "deploy", "deploy", nil); o[0].T != "h — deploy" {
		t.Errorf("op==opClass target = %q, want 'h — deploy'", o[0].T)
	}
}
