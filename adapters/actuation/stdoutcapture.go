package actuation

import (
	"context"
	"strings"
)

// StdoutCapture wraps an Actuator to record the (trimmed) stdout its Exec returned — the job handle an ASYNC
// launch answers with (the AWX job id, the GitOps MR reference; TG-122 slice 0, spec/017 REQ-1709). It adds
// NO behaviour: Exec delegates verbatim, Capability/ReadOnly pass through, and nothing here can reach an
// effect on its own — it observes the one Exec its caller's interceptor performs. It lives in this package
// (not core/regime) because the regime composition guard rightly forbids any Exec call there: the delegation
// belongs beside the Actuator contract itself.
//
// Not safe for concurrent Exec calls; the interceptor performs exactly one.
type StdoutCapture struct {
	leaf     Actuator
	captured string
}

// NewStdoutCapture wraps leaf. A nil leaf yields a capture whose Exec fails the same way the wrapped call
// would — callers hand it straight to an interceptor, which self-tests its actuator anyway.
func NewStdoutCapture(leaf Actuator) *StdoutCapture { return &StdoutCapture{leaf: leaf} }

func (s *StdoutCapture) Capability() string { return s.leaf.Capability() }
func (s *StdoutCapture) ReadOnly() bool     { return s.leaf.ReadOnly() }

// Exec delegates verbatim, recording the trimmed stdout of a SUCCESSFUL call (an errored launch produced no
// handle worth binding).
func (s *StdoutCapture) Exec(ctx context.Context, argv []string, stdin []byte) (Result, error) {
	res, err := s.leaf.Exec(ctx, argv, stdin)
	if err == nil {
		s.captured = strings.TrimSpace(string(res.Stdout))
	}
	return res, err
}

// Captured returns the trimmed stdout of the one delegated Exec — empty before any call and after an errored
// one.
func (s *StdoutCapture) Captured() string { return s.captured }
