---
name: escalation-boundary
class: skill
version: 0.1.0-distilled
source: distill:openclaw/skills/escalate-to-claude/SKILL.md
description: Escalate implementation and mutation work, answer information questions yourself from the source of truth
---

## Goal
Route work to the right tier: escalate what needs implementation or mutation; answer information
questions yourself, from the source of truth. Escalating a lookup wastes the expensive tier; answering
an implementation request inline produces half-built changes nobody owns.

## Required evidence
- The request, classified: does it ask for INFORMATION (status, details, history, identity) or for
  CHANGE (implement, build, fix, modify, escalate)?
- For information requests: the live read that answers it — `get-tracker-history` for work-item state,
  `get-estate-context` for identity, `get-device-status` for current state.
- For escalations: the incident identifier and its current state.

## Decision rules
- ESCALATE: implementation, refactoring, debugging of code, architecture decisions, any mutation of a
  system — and explicit requests to escalate. Do not answer these with partial implementations,
  analysis, or advice; route them whole.
- DO NOT ESCALATE information requests: "what is <host>", "status of this incident", "has this happened
  before" are lookups. Run the read and answer from its output. Never answer from memory, and never
  answer "not found in my memory" — memory is stale; the source of truth is the tool.
- No incident identifier on an escalation-worthy request → resolve or create the incident FIRST, then
  escalate it. An escalation without a record is unfindable work.
- ONE escalation per incident. Asked again, report the existing escalation and its state instead of
  firing a duplicate.
- Confirm an escalation by its recorded outcome — the state transition or session identifier from
  `get-tracker-history` — never by asserting "escalated" without it.

## Verification
- Every escalated incident shows the handoff in its state history, and the escalation reply cites that
  identifier.
- Every information request was answered with the tool's output quoted, not summarized from recall.
- The record contains no duplicate escalations for one incident, and no implementation fragments
  attached to requests that were routed onward.
