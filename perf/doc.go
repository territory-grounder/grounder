// Package perf holds cross-package PERFORMANCE benchmarks that consume other packages through their PUBLIC
// API only. It lives outside the decision-core (core/policy, core/risk, ...) on purpose: a benchmark is a
// black-box measurement of a component, never part of its law surface, so it must not sit behind the
// protected-path owner-approval gate that guards the decision-core itself. Keeping it here also keeps a perf
// change from ever masquerading as a law-surface change. The measured code is reached only through exported
// symbols, so these benchmarks can never alter a governed behavior.
package perf
