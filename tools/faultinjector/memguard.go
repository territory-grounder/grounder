package faultinjector

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The memory guard (TG-710) decides, for a target on a GUARDED Proxmox node, whether the node has room to take
// the target's memory back when the fault is restored. A whole-guest fault frees the guest's memory; anything
// else on the node may take it while the fault is held; the restore must then start the guest again and can
// never be refused. The only safe place to act is before the fault, so the guard admits a fault only when the
// node's FRESH telemetry shows the floor plus the declared restore cost of this fault and of every fault already
// outstanding on the node. It is a CHECK, not a reservation: nothing stops another workload from taking the
// memory after the check, and the guard never touches a restore or a compensation.

// SkipStage names where in the injection path the guard refused a target.
type SkipStage string

const (
	// SkipAtPlan is a refusal while the planner chose among the candidates; the planner moves on to the next one.
	SkipAtPlan SkipStage = "plan"
	// SkipBeforeEffect is a refusal on the second, fresh reading taken immediately before the obligation is
	// recorded and the effect runs; nothing is recorded and nothing is touched.
	SkipBeforeEffect SkipStage = "pre-effect"
)

// SkipReason is the closed set of reasons the guard refuses a target.
type SkipReason string

const (
	SkipTelemetryUnavailable   SkipReason = "telemetry-unavailable"
	SkipTelemetryStale         SkipReason = "telemetry-stale"
	SkipMemoryPressure         SkipReason = "memory-pressure"
	SkipBelowThreshold         SkipReason = "below-threshold"
	SkipNoDeclaredCost         SkipReason = "no-declared-cost"
	SkipOutstandingWithoutCost SkipReason = "outstanding-without-cost"
	SkipObligationsUnreadable  SkipReason = "obligations-unreadable"
)

// GuardSkip is one refused target, recorded apart from the injection ledger: a skipped target was never
// faulted, so it is in no fault denominator, and the record says why it was passed over.
type GuardSkip struct {
	Host            string
	Class           Class
	Node            string
	Stage           SkipStage
	Reason          SkipReason
	Detail          string
	HasTelemetry    bool
	AvailableBytes  int64
	PSISomeAvg60    float64
	TelemetryReadAt time.Time
	RequiredBytes   int64
	Note            string
}

// SkipRecorder is where refused targets are written. `*Store` writes them to injection_skip.
type SkipRecorder interface {
	RecordSkip(ctx context.Context, s GuardSkip) error
}

// NodeGuard is one guarded node's configuration. Every field is declared explicitly in the guard file.
type NodeGuard struct {
	Node            string
	FloorBytes      int64
	MaxAge          time.Duration
	MaxPSISomeAvg60 float64
}

// NodeTelemetry is one reading of a node's memory: the kernel's MemAvailable and the memory pressure some-avg60,
// taken over the injector's own SSH identity. ReadAt is the injector's own clock at the instant BEFORE the read was
// issued, so its age includes the whole SSH round trip and is never understated; the node's clock is not used.
type NodeTelemetry struct {
	Node           string
	AvailableBytes int64
	PSISomeAvg60   float64
	ReadAt         time.Time
	Err            error
}

type guardKey struct {
	host  string
	class Class
}

// MemGuard is the loaded guard configuration.
type MemGuard struct {
	nodes    map[string]NodeGuard
	costs    map[guardKey]int64
	hostNode map[string]string
}

const (
	bytesPerGiB         = 1 << 30
	maxTelemetryMaxAge  = 5 * time.Minute
	futureReadTolerance = 5 * time.Second
)

// TelemetryArgv is the fixed read the guard runs on a guarded node.
func TelemetryArgv() []string { return []string{"cat", "/proc/meminfo", "/proc/pressure/memory"} }

// LoadMemGuard reads a guard file.
func LoadMemGuard(path string) (*MemGuard, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("memory guard: %w", err)
	}
	defer f.Close()
	return ParseMemGuard(f)
}

// ParseMemGuard parses the guard file format:
//
//	node <name> floor-gib=<n> max-age=<duration> psi-some-avg60-max=<n>
//	cost <guest> <class> <gib>
//
// Every node attribute is required, every number must be positive, and nothing has a default: an unstated
// threshold would be a threshold nobody chose.
func ParseMemGuard(r io.Reader) (*MemGuard, error) {
	g := &MemGuard{nodes: map[string]NodeGuard{}, costs: map[guardKey]int64{}}
	known := map[Class]bool{}
	for _, c := range AllClasses() {
		known[c] = true
	}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		f := strings.Fields(text)
		switch f[0] {
		case "node":
			ng, err := parseNodeLine(f)
			if err != nil {
				return nil, fmt.Errorf("memory guard line %d: %w", line, err)
			}
			if _, dup := g.nodes[ng.Node]; dup {
				return nil, fmt.Errorf("memory guard line %d: node %s declared twice", line, ng.Node)
			}
			g.nodes[ng.Node] = ng
		case "cost":
			if len(f) != 4 {
				return nil, fmt.Errorf("memory guard line %d: want \"cost <guest> <class> <gib>\"", line)
			}
			c := Class(f[2])
			if !known[c] {
				return nil, fmt.Errorf("memory guard line %d: unknown class %q", line, f[2])
			}
			b, err := parseGiB(f[3])
			if err != nil {
				return nil, fmt.Errorf("memory guard line %d: cost: %w", line, err)
			}
			k := guardKey{host: f[1], class: c}
			if _, dup := g.costs[k]; dup {
				return nil, fmt.Errorf("memory guard line %d: cost for %s %s declared twice", line, f[1], c)
			}
			g.costs[k] = b
		default:
			return nil, fmt.Errorf("memory guard line %d: unknown directive %q", line, f[0])
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("memory guard: %w", err)
	}
	if len(g.nodes) == 0 {
		return nil, fmt.Errorf("memory guard: no node declared")
	}
	return g, nil
}

func parseNodeLine(f []string) (NodeGuard, error) {
	if len(f) != 5 {
		return NodeGuard{}, fmt.Errorf("want \"node <name> floor-gib=<n> max-age=<duration> psi-some-avg60-max=<n>\"")
	}
	ng := NodeGuard{Node: f[1]}
	seen := map[string]bool{}
	for _, kv := range f[2:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || seen[k] {
			return NodeGuard{}, fmt.Errorf("bad or repeated attribute %q", kv)
		}
		seen[k] = true
		switch k {
		case "floor-gib":
			b, err := parseGiB(v)
			if err != nil {
				return NodeGuard{}, fmt.Errorf("floor-gib: %w", err)
			}
			ng.FloorBytes = b
		case "max-age":
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 || d > maxTelemetryMaxAge {
				return NodeGuard{}, fmt.Errorf("max-age %q must be a duration in (0, %s]", v, maxTelemetryMaxAge)
			}
			ng.MaxAge = d
		case "psi-some-avg60-max":
			p, err := strconv.ParseFloat(v, 64)
			if err != nil || !(p > 0) || math.IsInf(p, 0) {
				return NodeGuard{}, fmt.Errorf("psi-some-avg60-max %q must be a positive number", v)
			}
			ng.MaxPSISomeAvg60 = p
		default:
			return NodeGuard{}, fmt.Errorf("unknown attribute %q", k)
		}
	}
	if !seen["floor-gib"] || !seen["max-age"] || !seen["psi-some-avg60-max"] {
		return NodeGuard{}, fmt.Errorf("node %s must declare floor-gib, max-age and psi-some-avg60-max", ng.Node)
	}
	return ng, nil
}

func parseGiB(s string) (int64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || !(v > 0) || math.IsInf(v, 0) || v > 4096 {
		return 0, fmt.Errorf("%q must be a positive number of GiB", s)
	}
	return int64(math.Round(v * bytesPerGiB)), nil
}

// Guards reports whether the node is guarded.
func (m *MemGuard) Guards(node string) bool {
	if m == nil {
		return false
	}
	_, ok := m.nodes[node]
	return ok
}

// Nodes lists the guarded nodes, sorted.
func (m *MemGuard) Nodes() []string {
	var out []string
	for n := range m.nodes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// DeclaredFor reports whether a class can act on the guest at all by the pool's declarations, mirroring the
// planner's own eligibility arms, so Validate demands a cost exactly where the planner could pick the pair.
func DeclaredFor(g PoolGuest, c Class) bool {
	switch c {
	case ClassServiceDown, ClassServiceHang:
		return g.Unit != ""
	case ClassContainerDown, ClassContainerHang:
		return g.Container != ""
	case ClassLogFill:
		return ValidLogPath(g.LogPath) == nil
	default:
		return true
	}
}

// Validate refuses a guard that disagrees with the pool or the rotation: a cost for a guest that is not a pool
// guest on a guarded node, or a guarded pool guest with no cost for a rotation class it is eligible for. Either
// gap would make the guard silently skip a target for the whole run, or silently admit one nobody sized.
//
// It also records each pool guest's owning node, so an outstanding fault whose recorded node is empty is still
// counted against the node that owns its guest.
func (m *MemGuard) Validate(pool []PoolGuest, rotation []Class) error {
	onGuarded := map[string]bool{}
	m.hostNode = map[string]string{}
	for _, g := range pool {
		m.hostNode[g.Name] = g.Node
		if m.Guards(g.Node) {
			onGuarded[g.Name] = true
		}
	}
	var problems []string
	for k := range m.costs {
		if !onGuarded[k.host] {
			problems = append(problems, fmt.Sprintf("cost for %s %s, which is not a pool guest on a guarded node", k.host, k.class))
		}
	}
	seen := map[Class]bool{}
	for _, c := range rotation {
		if seen[c] {
			continue
		}
		seen[c] = true
		for _, g := range pool {
			if !m.Guards(g.Node) || !DeclaredFor(g, c) {
				continue
			}
			if _, ok := m.costs[guardKey{host: g.Name, class: c}]; !ok {
				problems = append(problems, fmt.Sprintf("%s on guarded node %s has no cost for rotation class %s", g.Name, g.Node, c))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("memory guard does not match the pool and rotation: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Describe renders the effective configuration, one line per node and per cost, for the boot log and the
// registration.
func (m *MemGuard) Describe() []string {
	var out []string
	for _, n := range m.Nodes() {
		ng := m.nodes[n]
		out = append(out, fmt.Sprintf("node %s floor=%.2f GiB max-age=%s psi-some-avg60-max=%.2f", n, float64(ng.FloorBytes)/bytesPerGiB, ng.MaxAge, ng.MaxPSISomeAvg60))
	}
	var keys []guardKey
	for k := range m.costs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].host != keys[j].host {
			return keys[i].host < keys[j].host
		}
		return keys[i].class < keys[j].class
	})
	for _, k := range keys {
		out = append(out, fmt.Sprintf("cost %s %s %.2f GiB", k.host, k.class, float64(m.costs[k])/bytesPerGiB))
	}
	return out
}

// Evaluate decides whether the guard admits a fault of class c on guest g. A guest on an unguarded node is always
// admitted. On a guarded node the fault is admitted only when the telemetry is present, no older than the node's
// max-age, not under memory pressure, and shows MemAvailable of at least the floor plus this fault's declared
// restore cost plus the declared restore cost of every fault still outstanding on the same node. ok=false always
// carries the skip that says why.
func (m *MemGuard) Evaluate(now time.Time, g PoolGuest, c Class, outstanding []Outstanding, tel NodeTelemetry) (bool, GuardSkip) {
	skip := GuardSkip{Host: g.Name, Class: c, Node: g.Node}
	if !m.Guards(g.Node) {
		return true, skip
	}
	ng := m.nodes[g.Node]
	cost, ok := m.costs[guardKey{host: g.Name, class: c}]
	if !ok {
		skip.Reason, skip.Detail = SkipNoDeclaredCost, fmt.Sprintf("no declared restore cost for %s %s on guarded node %s", g.Name, c, g.Node)
		return false, skip
	}
	if tel.Err != nil || tel.ReadAt.IsZero() {
		why := "no reading"
		if tel.Err != nil {
			why = tel.Err.Error()
		}
		skip.Reason, skip.Detail = SkipTelemetryUnavailable, fmt.Sprintf("%s telemetry unavailable: %s", g.Node, why)
		return false, skip
	}
	skip.HasTelemetry, skip.AvailableBytes, skip.PSISomeAvg60, skip.TelemetryReadAt = true, tel.AvailableBytes, tel.PSISomeAvg60, tel.ReadAt
	if age := now.Sub(tel.ReadAt); age > ng.MaxAge || age < -futureReadTolerance {
		skip.Reason, skip.Detail = SkipTelemetryStale, fmt.Sprintf("%s telemetry is %s old (max-age %s)", g.Node, age.Round(time.Millisecond), ng.MaxAge)
		return false, skip
	}
	if tel.PSISomeAvg60 >= ng.MaxPSISomeAvg60 {
		skip.Reason, skip.Detail = SkipMemoryPressure, fmt.Sprintf("%s memory pressure some-avg60 %.2f is at or above %.2f", g.Node, tel.PSISomeAvg60, ng.MaxPSISomeAvg60)
		return false, skip
	}
	need := ng.FloorBytes + cost
	held := 0
	for _, o := range outstanding {
		node := o.Node
		if node == "" {
			node = m.hostNode[o.Host]
		}
		if node == "" {
			if _, sized := m.costs[guardKey{host: o.Host, class: o.Class}]; sized {
				node = g.Node
			}
		}
		if node != g.Node {
			continue
		}
		oc, ok := m.costs[guardKey{host: o.Host, class: o.Class}]
		if !ok {
			skip.Reason, skip.Detail = SkipOutstandingWithoutCost, fmt.Sprintf("outstanding fault id=%d (%s %s) on %s has no declared restore cost", o.ID, o.Host, o.Class, g.Node)
			return false, skip
		}
		need += oc
		held++
	}
	skip.RequiredBytes = need
	if tel.AvailableBytes < need {
		skip.Reason, skip.Detail = SkipBelowThreshold, fmt.Sprintf("%s has %.2f GiB available; %s %s needs %.2f GiB (floor %.2f + this restore %.2f + %d outstanding restore(s))",
			g.Node, float64(tel.AvailableBytes)/bytesPerGiB, g.Name, c, float64(need)/bytesPerGiB, float64(ng.FloorBytes)/bytesPerGiB, float64(cost)/bytesPerGiB, held)
		return false, skip
	}
	return true, skip
}

// ReadNodeTelemetry takes one reading of a node's memory over the runner. Any transport failure, non-zero exit or
// unparseable output is returned in Err, which the guard reads as unavailable.
func ReadNodeTelemetry(ctx context.Context, run Runner, node string, now func() time.Time) NodeTelemetry {
	t := NodeTelemetry{Node: node, ReadAt: now().UTC()}
	out, code, err := run.Run(ctx, node, TelemetryArgv())
	switch {
	case err != nil:
		t.Err = fmt.Errorf("read: %w", err)
	case code != 0:
		t.Err = fmt.Errorf("read exited %d", code)
	default:
		t.AvailableBytes, t.PSISomeAvg60, t.Err = parseNodeTelemetry(out)
	}
	if t.Err != nil {
		t.ReadAt = time.Time{}
	}
	return t
}

func parseNodeTelemetry(out string) (int64, float64, error) {
	var avail int64 = -1
	psi := -1.0
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch {
		case f[0] == "MemAvailable:":
			if len(f) != 3 || f[2] != "kB" {
				return 0, 0, fmt.Errorf("malformed MemAvailable line %q", line)
			}
			kb, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil || kb < 0 {
				return 0, 0, fmt.Errorf("malformed MemAvailable value %q", f[1])
			}
			avail = kb * 1024
		case f[0] == "some":
			for _, kv := range f[1:] {
				if v, ok := strings.CutPrefix(kv, "avg60="); ok {
					p, err := strconv.ParseFloat(v, 64)
					if err != nil || p < 0 || math.IsNaN(p) || math.IsInf(p, 0) {
						return 0, 0, fmt.Errorf("malformed memory pressure avg60 %q", v)
					}
					psi = p
				}
			}
		}
	}
	if avail < 0 {
		return 0, 0, fmt.Errorf("no MemAvailable in the reading")
	}
	if psi < 0 {
		return 0, 0, fmt.Errorf("no memory pressure some avg60 in the reading")
	}
	return avail, psi, nil
}
