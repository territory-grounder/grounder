// Package acceptance holds the executable acceptance oracles for spec/025 (the benchmark harness — the
// measurement plane that produces the v1.0 claim). The scenarios assert the properties that make a published
// number defensible: one lockstep-bound computation per axis, golden fixtures over a REAL database whose
// mutation control proves they constrain the SQL rather than track it, populations reported with their
// denominators and exclusions, and comparison restricted to like-for-like ground.
//
// Read-only: nothing here actuates, gates a decision, or writes a governed table.
package acceptance
