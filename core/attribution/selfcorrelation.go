package attribution

// SELF-CORRELATION (TG-684 for the journal domain, TG-693 for pve; owner ruling 2026-11-07). A record whose actor is
// TG's own actuation identity counts as TG's ONLY when TG's own execution record explains it: a credential is not
// proof of who used it. Each domain reader finds its own records and reads its own execution times; the pairing rule
// is the same everywhere, so it lives here once.

import (
	"sort"
	"time"
)

// SelfRecords returns the indexes of the records in out whose actor is self, oldest first. self == "" matches nothing.
func SelfRecords(out []Evidence, self string) []int {
	if self == "" {
		return nil
	}
	var mine []int
	for i := range out {
		if out[i].Actor == self {
			mine = append(mine, i)
		}
	}
	sort.SliceStable(mine, func(a, b int) bool { return out[mine[a]].ObservedAt.Before(out[mine[b]].ObservedAt) })
	return mine
}

// PairSelf marks every record named by mine (as SelfRecords returns them, oldest first) SelfCorrelated or
// SelfUnexplained.
//
// ONE RECORD PER EXECUTION. One TG execution produces exactly one record in each domain it touches (one sshd login
// for an SSH-lane heal, one task for a Proxmox start), so an execution explains at most ONE record: a second record
// with TG's identity inside the same window — a credential holder riding on a real heal — reads unexplained. Records
// are matched in time order to the earliest unused execution with executed_at − record in [−before, +after], which
// pairs every record an honest one-per-execution record can.
func PairSelf(out []Evidence, mine []int, executions []time.Time, before, after time.Duration) {
	times := append([]time.Time(nil), executions...)
	sort.Slice(times, func(a, b int) bool { return times[a].Before(times[b]) })
	used := make([]bool, len(times))
	for _, i := range mine {
		if j := unusedExecutionFor(times, used, out[i].ObservedAt, before, after); j >= 0 {
			used[j] = true
			out[i].SelfCorrelated = true
		} else {
			out[i].SelfUnexplained = true
		}
	}
}

// unusedExecutionFor returns the earliest execution not yet claimed by another record that lies in
// [at − before, at + after], or -1. executions must be sorted ascending.
func unusedExecutionFor(executions []time.Time, used []bool, at time.Time, before, after time.Duration) int {
	for j, e := range executions {
		if used[j] {
			continue
		}
		if d := e.Sub(at); d >= -before && d <= after {
			return j
		}
	}
	return -1
}
