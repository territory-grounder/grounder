---
name: security-deep-verification
class: runbook
version: 0.1.0-distilled
source: distill:.claude/agents/security-analyst.md
description: The escalated tier a security finding hands into — verify exposure from an untrusted vantage, enrich with threat context, size blast radius, time-box remediation
---

## Goal
The deep-verification protocol a triaged security finding escalates INTO (security-finding-triage is
the triage seat; this is the receiving tier): establish real exposure, enrich with threat context, and
size the blast radius — with evidence, not scanner output alone. NOTE — tool-gated content: TG has no
scanner, threat-intel, or perimeter-ACL read tools wired; this runbook is knowledge-library material
until vendor-official, read-only surfaces land (per ADR-0012).

## Required evidence
- The triage handoff: finding identifier, scanner context, target identity already resolved to
  host + service, and the specific verification requested.
- An exposure probe from OUTSIDE the trusted path: internal scanners often traverse tunnels and
  whitelisted source addresses — what they can reach says nothing about what the internet can reach.
  The probe's vantage is recorded with the result.
- Threat context for the finding: exploitation likelihood (exploit-prediction scores where available),
  mass-scanning reputation of any source addresses, and whether the finding sits in the accepted-risk
  baseline.
- The LIVE device or service config for any control being reasoned about — running config is the only
  truth; a backed-up or repository copy proves nothing about now.
- `get-estate-context <host>` for the target's neighbors and what lateral movement could reach.

## Decision rules
- "Scanner-visible" and "publicly exposed" are different claims and only the second sets the SLA:
  verify exposure from an untrusted vantage before priority is assigned.
- Scanner severity is an input, not the verdict: exposure context (internet-facing vs internal,
  authenticated vs open) and exploitation likelihood move real priority in both directions. Record
  the reasoning that moved it.
- The remediation ladder is time-boxed by verified severity — a critical, internet-exposed, likely-
  exploited finding is measured in hours; an internal, ACL-shielded one in review cycles. State the
  box chosen and why.
- Blast radius is stated as a claim about the estate — single host, segment, site, cross-site — and
  cites the topology evidence, not intuition.
- Never mass-delete config lines matched by a pattern: audit each line individually. A pattern that
  matches the offending lines also matches load-bearing ones; the mass-delete that "cleaned up" a
  ruleset has broken production before.
- The accepted-risk baseline changes only through its owning review; deep verification produces a
  RECOMMENDATION to accept or fix, never a silent baseline write (the triage seat's rule, unchanged
  at this tier).

## Verification
- The exposure verdict names its vantage point and shows the probe result.
- Every remediation recommendation carries the verified severity, the time box, and the evidence
  chain from finding to exposure to impact.
- The live-config reads in the record are timestamped from the device, not quoted from a repository.
