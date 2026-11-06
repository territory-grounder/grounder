---
name: k8s-service-ingress-dns-triage
class: runbook
version: 0.1.0-distilled
source: distill:https://kubernetes.io/docs/tasks/debug/debug-application/debug-service/
description: Kubernetes Service, Ingress, and CoreDNS triage — work the DNS-to-Endpoints-to-Ingress chain in order, not the layer you suspect first
---

## Goal
Work a Service/Ingress/CoreDNS "can't reach it" symptom down the actual delivery chain — DNS resolution,
then Service-to-Endpoint wiring, then Ingress-to-Service routing — instead of guessing at whichever layer
seems most likely first. NOTE — tool-gated content: TG has no Kubernetes read tool wired into the agent's
own tool registry (`agent/tools.go`) — `modules/actuation/kubernetes` exists as a declared-but-disabled
reference actuation module (Phase 0/1, a `deniedRunner`, no execution path; see
`modules/bootstrap/bootstrap.go`); this runbook is knowledge-library material until a vendor-official,
read-only cluster surface is wired into the agent's own tools. Every command below is read-only diagnostic
guidance.

## Required evidence
- `kubectl get svc <name> -n <namespace> -o json` (or `describe`) — `spec.selector`,
  `spec.ports[].port`/`targetPort`, `spec.type`, `spec.clusterIP`.
- `kubectl get endpointslices -l kubernetes.io/service-name=<name> -n <namespace>` — whether the Service
  resolved any Pod addresses at all; an empty or absent EndpointSlice means the selector matched zero READY
  pods, independent of whether those pods look fine by other measures.
- `kubectl get pods -l <the Service's own selector> -n <namespace> -o wide` — confirms the label match and
  that matching pods are `Running` AND `Ready` (a pod that is `Running` but failing its readiness probe is
  excluded from Endpoints even though `get pods` shows it).
- DNS-specific: a disposable test pod (`kubectl run -it --rm --restart=Never busybox --image=<busybox-image>
  -- sh`, or the published `dnsutils` example manifest) to run `nslookup <service>.<namespace>.svc.cluster.local`
  and `cat /etc/resolv.conf` from inside the cluster network; `kubectl get pods -n kube-system -l
  k8s-app=kube-dns` and `kubectl logs -n kube-system -l k8s-app=kube-dns` for CoreDNS's own health.
- Ingress-specific: `kubectl get ingress <name> -n <namespace>` (whether the `ADDRESS` column is populated)
  and `kubectl describe ingress <name> -n <namespace>` — the `Rules:` table (Host/Path/Backends) and
  `Events:`.

## Decision rules
- Work the chain in order: DNS resolves a name to a ClusterIP, the Service's Endpoints/EndpointSlice map
  that ClusterIP to Pod addresses, kube-proxy programs the actual packet forwarding, and — if applicable —
  the Ingress controller routes external traffic to the Service. A failure at any layer produces the same
  user-facing symptom ("can't reach it"), so confirm each layer's OWN evidence instead of assuming the one
  that seems most likely.
- An EndpointSlice with zero addresses for a Service whose selector LOOKS right is a label mismatch until
  proven otherwise — diff the Service's `spec.selector` against the pods' actual `metadata.labels` directly.
  A typo'd or stale label is the single most common cause, and `kubectl get pods -l <selector>` either
  returns the expected pods or it doesn't.
- A pod can be `Running` and still excluded from a Service's Endpoints if it is not `Ready` — a failing
  readiness probe removes it from load-balancing without restarting it (unlike a liveness-probe failure).
  The `READY` column from `kubectl get pods` (for example `0/1`) catches this where `STATUS: Running` alone
  does not.
- `port` versus `targetPort` confusion reads as "the Service exists, nothing answers": `port` is what
  clients call, `targetPort` is the container's actual listening port. A Service can be perfectly
  well-formed and still route to a port nothing inside the container is bound to.
- DNS failing while a direct Pod-address request succeeds isolates the fault to DNS specifically (CoreDNS
  pods not `Running`, or the `kube-dns`/`coredns` Service itself has no working endpoints) rather than to
  networking generally — test both rather than assuming one implicates the other.
- For Ingress: an empty `ADDRESS` in `kubectl get ingress` means the Ingress controller has not
  admitted/provisioned it yet — check that a controller is running and that the Ingress's
  `spec.ingressClassName` matches an IngressClass it actually watches. This is a controller-admission
  problem, upstream of anything in the `Rules:` table; no amount of editing hosts or paths will populate an
  address the controller never assigned.

## Verification
- The disposable test pod's `nslookup` resolves the Service name to the expected ClusterIP, AND a request
  against that ClusterIP (or the Ingress `ADDRESS`) succeeds — a name that only resolves, or an address that
  only answers when hit directly, is a partial fix.
- `kubectl get endpointslices -l kubernetes.io/service-name=<name>` shows the expected number of addresses,
  matching the count of Ready pods under the selector.
- `kubectl describe ingress` shows the expected `ADDRESS`, and its `Events:` carry no unresolved warning as
  the latest entry.

## Doc basis
- Kubernetes: Debug Services —
  https://kubernetes.io/docs/tasks/debug/debug-application/debug-service/
  (the DNS-then-EndpointSlice-then-Pod-then-kube-proxy troubleshooting ladder; selector, port, and
  targetPort checks).
- Kubernetes: Debugging DNS Resolution —
  https://kubernetes.io/docs/tasks/administer-cluster/dns-debugging-resolution/
  (the `dnsutils` test pod, `nslookup`, `/etc/resolv.conf`, CoreDNS pod/Service/logs checks).
- Kubernetes: DNS for Services and Pods —
  https://kubernetes.io/docs/concepts/services-networking/dns-pod-service/
  (A/AAAA/SRV record formation for Services, `dnsPolicy` values).
- Kubernetes: Ingress —
  https://kubernetes.io/docs/concepts/services-networking/ingress/
  (`ingressClassName`, rules/paths/backend mapping, `kubectl describe ingress` fields including `Address`).
