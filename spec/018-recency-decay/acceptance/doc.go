// Package acceptance holds the executable acceptance oracles for spec/018 (shared recency, decay & periodic
// reconciliation of the learned stores). Every scenario drives the already-implemented real code —
// core/lessons provenance + decay, core/learn half-life, core/estate decay-on-disproof, and the discovery
// -corpus drain through core/falsify + eval — so none is @pending. This is competence-plane maintenance: it
// ages LEARNED state only; it never touches the estate, actuates, or gates, and mutation stays OFF.
package acceptance
