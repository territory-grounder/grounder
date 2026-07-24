// Package temporal holds Territory Grounder's Temporal conventions: task-queue names, the
// single-org workflow-id scheme, and reuse policy.
//
// Provenance: [corrections] substrate migration — Temporal replaces the predecessor's n8n engine +
// Cronicle scheduler + most of the watchdog/reconcile machinery · [R] paradigm-rule 7, "session
// orchestrator", tension "session concurrency" (fixed 5-slots → workers/task-queues), P0-7 ·
// [R] ADR-0010 (single-org: workflow ids are session-scoped, not tenant-scoped).
//
// Phase 0 establishes the conventions and stands up the Temporal service in docker-compose. The live
// Go worker and the Runner workflow are wired in P1-7 (importing go.temporal.io/sdk), so the heavy
// SDK dependency lands with the code that uses it, not in the P0 skeleton.
package temporal

import "fmt"

// Task queues. One per logical worker pool; concurrency is a queue/worker option, not a fixed slot
// count.
const (
	TaskQueueRunner   = "tg.runner"   // the session Runner workflow (P1-7)
	TaskQueueSchedule = "tg.schedule" // periodic read-only jobs (P1-9), replaces Cronicle
)

// WorkflowID returns the session-scoped id "tg/{session}". One organization, one estate (ADR-0010),
// so the session id alone isolates a run and enables SignalWithStart routing to the exact owning
// workflow (P1-8). session must be a validated, non-empty identifier.
func WorkflowID(session string) string {
	return fmt.Sprintf("tg/%s", session)
}

// ScheduleID returns the id for a periodic schedule.
func ScheduleID(name string) string {
	return fmt.Sprintf("tg/sched/%s", name)
}

// WorkflowIDReusePolicy: TG uses "reject-duplicate" semantics so a second trigger for an in-flight
// session idempotently attaches rather than forking a duplicate run. The concrete enterprise policy
// constant is applied where the SDK client is constructed (P1-7).
const WorkflowIDReusePolicy = "RejectDuplicate"
