package runner

// THE LADDER CREDIT RUNS ON THE TRIAGE QUEUE (TG-321).
//
// RegisterActuationActivities states its own completeness claim in a comment, and that claim is what makes
// this gap legible instead of arguable:
//
//	"ExecuteActivity is the whole list, and that is a claim worth stating rather than assuming: it is the
//	 only Runner activity that traverses the interceptor chain to an effect leaf ... i.e. the only one that
//	 can reach sshactuation/awxjob/proxmox with a write credential."
//
// GraduationActivity is therefore registered on the TRIAGE side. The TG-164 database plane split stops a
// compromised triage worker forging the RECORD of an actuation; it does not stop that worker advancing the
// ladder that decides which op-classes may auto-actuate in future. That is a slower, quieter path to the
// same place — rather than forging one execution, a popped triage worker earns an op-class into AUTO.
//
// THIS TEST DOES NOT ASSERT THE GAP IS CLOSED. It pins the registration split so the property is stated in
// executable form rather than living only in a comment and a ticket, and so that MOVING GraduationActivity
// onto the actuation queue is a deliberate, visible act rather than a silent one — in either direction.
// Closing TG-321 means changing this test on purpose.

import (
	"os"
	"strings"
	"testing"
)

func TestActuationRegistrationSetIsExactlyExecuteActivity(t *testing.T) {
	raw, err := os.ReadFile("register.go")
	if err != nil {
		t.Fatalf("read register.go: %v", err)
	}
	src := string(raw)

	i := strings.Index(src, "func RegisterActuationActivities")
	if i < 0 {
		t.Fatal("RegisterActuationActivities not found — this guard is reading a file that no longer " +
			"splits the planes, and would otherwise pass by matching nothing")
	}
	block := src[i:]
	if end := strings.Index(block[1:], "\nfunc "); end > 0 {
		block = block[:end+1]
	}

	var registered []string
	for _, ln := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "//") || !strings.Contains(trimmed, "RegisterActivity(") {
			continue
		}
		if k := strings.Index(trimmed, "a."); k >= 0 {
			registered = append(registered, strings.TrimSuffix(trimmed[k+2:], ")"))
		}
	}

	if len(registered) == 0 {
		t.Fatal("RegisterActuationActivities registers NOTHING. Either the split was removed or this " +
			"guard's parse broke; both must fail loudly, because an empty actuation set means the " +
			"actuation worker polls a queue it can service no work on.")
	}
	// The actuation set is the list of activities reaching a process that holds the estate's write credential.
	// It is EXACTLY the two activities that traverse the interceptor chain to an effect leaf: ExecuteActivity
	// (the forward heal) and SealRollbackExecuteActivity (the TG-462 manual-rollback execute — same chain, with
	// InvertsActionID set). Widening it is a deliberate governance event; anything NOT on this list (e.g. the
	// ladder credit below) must stay off the actuation queue.
	wantActuation := map[string]bool{"ExecuteActivity": true, "SealRollbackExecuteActivity": true}
	if len(registered) != len(wantActuation) {
		t.Errorf("the actuation registration set is %v, want exactly [ExecuteActivity SealRollbackExecuteActivity].\n"+
			"That set is the list of activities reaching a process that holds the estate's write "+
			"credential. Adding to it widens what a popped ACTUATION worker can do; the comment above it "+
			"states the completeness claim, so any change here must update that claim too.", registered)
	}
	for _, name := range registered {
		if !wantActuation[name] {
			t.Errorf("unexpected activity %q on the actuation queue — the actuation set must be exactly "+
				"[ExecuteActivity SealRollbackExecuteActivity]; a new entry widens the write-credential surface "+
				"and must be a deliberate, reviewed addition", name)
		}
	}

	// The other half of the same fact: the ladder credit is NOT on the actuation queue. This is the
	// open finding, pinned so that closing it is deliberate.
	if strings.Contains(block, "GraduationActivity") {
		t.Errorf("GraduationActivity now registers on the ACTUATION queue. That may well be the right " +
			"fix for TG-321 — but it moves the ladder's earn path onto the plane holding the write " +
			"credential, which is a different trade, not a free one. Update TG-321 and this test together.")
	}
}
