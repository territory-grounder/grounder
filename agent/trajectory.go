package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// TrajectoryStep is one recorded action in the agent's investigation trajectory: the tool it called and a
// stable key of its arguments. Two identical steps mean the loop asked the same question twice — it is not
// learning from the observation it already has.
type TrajectoryStep struct {
	Tool    string
	ArgsKey string
}

// ArgsKey builds a stable, order-independent key from a tool's arguments, so the same call with the same args
// (in any map order) produces the same key.
func ArgsKey(args map[string]string) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\x00')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(args[k])
	}
	return b.String()
}

// Trajectory-veto thresholds. A stuck agent burns model spend re-asking a question it already answered; the
// veto halts it early instead of running out the full cycle budget.
const (
	// LoopThreshold is the number of IDENTICAL CONSECUTIVE steps that constitute a loop — the agent is
	// re-running the exact same call, ignoring the observation it just received.
	LoopThreshold = 3
	// RepeatThreshold is the number of times ANY single step may recur across the whole trajectory before it
	// is treated as thrashing (oscillating between the same few calls without converging).
	RepeatThreshold = 4
)

// TrajectoryVeto detects a stuck trajectory: either LoopThreshold identical consecutive steps, or a single
// step recurring RepeatThreshold times anywhere. It returns (true, reason) to veto the run — a deterministic
// analysis of the agent's OWN actions (INV-08: no model token is consulted), so a looping agent is halted
// before it exhausts its cycle budget re-asking the same question.
func TrajectoryVeto(steps []TrajectoryStep) (bool, string) {
	if len(steps) == 0 {
		return false, ""
	}
	run := 1
	for i := 1; i < len(steps); i++ {
		if steps[i] == steps[i-1] {
			run++
			if run >= LoopThreshold {
				return true, "loop: the same tool call was repeated " + itoa(run) + " times in a row, ignoring its observations"
			}
		} else {
			run = 1
		}
	}
	counts := make(map[TrajectoryStep]int, len(steps))
	for _, s := range steps {
		counts[s]++
		if counts[s] >= RepeatThreshold {
			return true, "thrash: the same tool call recurred " + itoa(counts[s]) + " times across the trajectory without converging"
		}
	}
	return false, ""
}

// HashedTrajectory returns the trajectory with each ArgsKey replaced by a short, stable digest, so the ordered
// tool path can be carried out of the activity and on to the eval scorer WITHOUT the raw argument values
// ArgsKey holds (host/target refs — INV-13 / the public-mirror rule). Equality is preserved (same tool + same
// args -> same digest), so a TrajectoryVeto over the digested steps detects exactly the same loops/thrash the
// runtime does over the raw ones (TG-525).
func HashedTrajectory(steps []TrajectoryStep) []TrajectoryStep {
	if len(steps) == 0 {
		return nil
	}
	out := make([]TrajectoryStep, len(steps))
	for i, s := range steps {
		out[i] = TrajectoryStep{Tool: s.Tool, ArgsKey: digestArgsKey(s.ArgsKey)}
	}
	return out
}

// digestArgsKey is a short, stable digest of an ArgsKey: "" stays "" (a no-arg call stays distinguishable from
// a hashed one), and any non-empty key becomes 12 hex chars of its sha256 — enough to keep distinct arg sets
// distinct within one session's trajectory, and it is one-way, so the raw values never travel.
func digestArgsKey(k string) string {
	if k == "" {
		return ""
	}
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:6])
}

// itoa is a tiny non-allocating-ish integer formatter (avoids pulling strconv into a hot safety path).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
