---
name: security-finding-triage
class: runbook
version: 0.1.0-distilled
source: distill:openclaw/skills/security-triage/SKILL.md
description: Triage a scanner finding by identity, novelty against baseline, and exposure — deep verification is escalated
---

## Goal
Triage a vulnerability-scanner finding by answering three questions in order: WHOSE is the scanned
target (identity), IS this new (novelty against baseline), and HOW exposed is it (context). Deep
verification is escalated, never performed from the triage seat. NOTE — tool-gated content: TG has no
security-scanner read tools wired; this runbook is knowledge-library material until a vendor-official,
read-only scanner surface lands (skills on unwired tools are not authored, per ADR-0012).

## Required evidence
- IDENTITY: the scanned <ip> resolved to its owning host and service via `get-estate-context` — a
  finding is meaningless until it has an owner.
- NOVELTY: the finding held against the accepted-findings baseline and `get-incident-history` — is
  this already known and accepted, or new?
- CONTEXT: which scanner produced it, the finding category (vulnerability, open port, transport
  security, header, exposure), the affected <port>, and the scan's own data for the target.
- For a NEW open-port finding: identification of the service actually listening there.

## Decision rules
- Identity first, always. A finding on an address the inventory cannot map is itself a finding — an
  inventory gap or an unknown listener — and outranks the original alert.
- Already-in-baseline → record the recurrence against the existing acceptance; no new incident. The
  baseline is the accepted-risk register, and re-triaging accepted risk is noise.
- NEW finding → order by severity: critical and high open an incident AND escalate deep verification
  (full re-scan, targeted probing, transport-security testing) to the tier equipped for it. The triage
  seat gathers context; it does not probe.
- A new open port is judged by WHAT is listening: an expected service newly exposed is a change-control
  question; an unknown listener is a possible compromise — identify before classifying.
- Scanner severity is an INPUT, not the verdict: exposure context (internet-facing or internal,
  authenticated or open) moves the real priority in both directions.
- The baseline changes only through its owning review — a triage session never quietly accepts a
  finding into it.

## Verification
- Every finding in the record resolves to host + service, cited from the inventory read.
- The new-vs-known decision is recorded WITH the baseline evidence consulted.
- Escalations carry the finding identifier, scanner context, and the specific verification requested.
- The accepted-findings baseline shows no entry that arrived outside its review path.
