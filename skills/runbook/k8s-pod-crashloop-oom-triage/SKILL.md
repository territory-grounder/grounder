---
name: k8s-pod-crashloop-oom-triage
class: runbook
version: 0.1.0-distilled
source: distill:https://kubernetes.io/docs/tasks/debug/debug-application/debug-running-pod/
description: Kubernetes CrashLoopBackOff and OOMKilled pod triage — exit code and Last State before a restart destroys the evidence, --previous logs first
---

## Goal
Attribute a CrashLoopBackOff or OOMKilled pod to its real cause — a bad exit, an out-of-memory kill, or a
failing probe — before restarting or deleting anything, because both actions destroy the exact evidence
that names the cause. NOTE — tool-gated content: TG has no Kubernetes read tool wired into the agent's own
tool registry (`agent/tools.go`) — `modules/actuation/kubernetes` exists as a declared-but-disabled
reference actuation module (Phase 0/1, a `deniedRunner`, no execution path; see
`modules/bootstrap/bootstrap.go`); this runbook is knowledge-library material until a vendor-official,
read-only cluster surface is wired into the agent's own tools. Every command below is read-only diagnostic
guidance.

## Required evidence
- `kubectl get pods -n <namespace>` — the `STATUS` column and the `RESTARTS` count; a climbing restart
  count against a fresh `AGE` is the CrashLoopBackOff signature itself.
- `kubectl describe pod <pod> -n <namespace>` — the `Containers:` block's `State`, `Last State`, `Reason`,
  `Exit Code`, and `Restart Count`, plus the `Events:` list (`Scheduled`, `Pulling`, `Pulled`, `Created`,
  `Started`, and a `Warning BackOff` entry once the restart throttle engages).
- `kubectl logs <pod> -n <namespace>` (the CURRENT container) and `kubectl logs <pod> -n <namespace>
  --previous` (the CRASHED instance's own log) — read both; the current container may be a fresh, healthy
  restart that explains nothing about why the last one died.
- `kubectl get pod <pod> -n <namespace> -o yaml`, specifically
  `status.containerStatuses[].lastState.terminated.{reason,exitCode,startedAt,finishedAt}` — the
  machine-readable form of what `describe` renders as prose, useful when the text output truncates.
- The container's declared `resources.limits.memory` from the pod spec, and, when the pod-level evidence
  is ambiguous, `kubectl describe node <node>` for a node-level `Warning OOMKilling` event — the kernel
  cgroup line naming the actual process the kernel killed and its OOM score.

## Decision rules
- Exit code first: `137` is SIGKILL (OOMKilled, or an external kill), `143` is SIGTERM (graceful
  termination — often a probe-triggered restart honoring `terminationGracePeriodSeconds`), `0` is a clean
  exit (a pod under `restartPolicy: Always` that exits `0` still loops — the application is treating "done"
  as a state to fix in the workload, not something the cluster is doing wrong). `Last State.Reason` names
  the cause directly when Kubernetes recognizes it (`OOMKilled` is stamped there, not inferred).
- Read `--previous` logs before anything else. Whatever explains the crash lives in the terminated
  instance's own log, not the running one's — and both the log and the exact exit reason are gone once the
  old container is garbage-collected past its retention window.
- CrashLoopBackOff is Kubernetes' own restart throttle (exponential backoff, capped at five minutes), not a
  distinct failure mode — it names only "this pod keeps crashing and is being backed off," never the
  underlying reason. Treat it as a symptom label, and read `Last State.Reason` for the diagnosis.
- `OOMKilled` at exit `137` with `resources.limits.memory` set means the container's actual usage exceeded
  that ceiling. Do not raise the limit reflexively — check the node-level `OOMKilling` event first; on a
  memory-pressured node, other pods sharing it can be affected too, and the fix may belong at a different
  layer (the workload's real footprint, not just its declared limit).
- A failing liveness probe restarts a container that may otherwise be running fine. This looks identical to
  a crash in restart count and `BackOff` events, but the exit reason typically traces to the probe's own
  timeout rather than an application fault — the pod spec's `livenessProbe` thresholds are what to revisit,
  not the application.
- Don't delete or force-restart the pod before capturing `--previous` logs and the exit reason — there is
  no way to recover either afterward.

## Verification
- `kubectl get pods -n <namespace>` shows `RESTARTS` not climbing across a fresh observation window, with
  `STATUS: Running` and the container marked Ready.
- `kubectl describe pod` on a retest shows the same `Last State` as before the fix — no new termination has
  been recorded — not just "no errors printed just now."
- If memory was the cause, confirm actual usage stays under the limit across at least one full traffic
  cycle, not a single low reading taken right after the fix.

## Doc basis
- Kubernetes: Debug Running Pods —
  https://kubernetes.io/docs/tasks/debug/debug-application/debug-running-pod/
  (`kubectl describe pod` / `kubectl logs` / `kubectl logs --previous` workflow; Last State, Reason, Exit
  Code fields; exit code 137 for OOMKilled).
- Kubernetes: Assign Memory Resources to Containers and Pods —
  https://kubernetes.io/docs/tasks/configure-pod-container/assign-memory-resource/
  (OOMKilled status, `lastState.terminated.reason`/`exitCode`, the node-level `OOMKilling` cgroup event).
- Kubernetes: Pod Lifecycle —
  https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/
  (container states, `restartPolicy`, the CrashLoopBackOff exponential-backoff behavior, probe semantics).
