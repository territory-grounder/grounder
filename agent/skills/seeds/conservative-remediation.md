## Conservative-remediation catalog (what to propose)
When the observations CONFIRM one of these causes, propose the single most conservative REVERSIBLE fix using
EXACTLY the op-class named — these execute after approval — and in your rationale state the
PREDICTION: the exact observation that will change when the fix works (the falsifiable check the verifier applies):
  Guest/VM/CT down, its host up      -> start-guest        (predict: guest status running, pings resume)
  Enabled service down or wedged     -> start-service if stopped, restart-service if wedged  (predict: is-active, health check passes)
  Config change needs applying       -> reload-service      (predict: the unit reports reloaded, config active)
  Container exited/crashlooping      -> start-container if exited, restart-container if wedged (predict: container Up, restarts stable)
  Guest disk full AND check-host-disk says the rootfs CAN be grown -> disk-grow (predict: df shows free space rose)
NO REGISTERED OP-CLASS addresses the cause? The duty is UNCHANGED: still name the fix. Emit the proposal with
a free-form op_class slug of your own — short, descriptive, hyphenated (e.g. renew-certificate,
unlock-resource, rotate-logfile) — plus the SAME rigor as any registered proposal: the exact target, a
rationale quoting the observed facts, a falsifiable prediction, AND an "undo_sketch": one sentence on how a
human would reverse the action. A free-form proposal is RECORDED for operator review and can NEVER execute
(nothing seals, every effect leaf refuses it), so propose the RIGHT fix — never substitute the nearest
registered op-class for the one you actually need, and never stand down just because the registry lacks the
verb. STOP remains correct ONLY when no safe conservative reversible action exists at all (e.g. disk filling
on a loopback rootfs with no identified safely-removable consumer: STOP naming the consuming path from
du/journal and the evidence a human needs).
ACTOR EVIDENCE (an authored stop by a named actor in the task log, a declared maintenance sentinel, a
declared chaos/benchmark window) NEVER suppresses the proposal: name the addressing fix anyway, cite that
evidence in the rationale, and let approval routing raise the bar — who approves is a separate question from
what the fix is.
HARD FLOOR — never propose these as an auto/reversible action (they require a human poll):
  - stateful workloads (etcd, *postgres*, *mysql*, *-db, redis, prometheus, seaweedfs, thanos): a restart can lose data.
  - host reboot / shutdown, P0-host power-cycle; guest reset/stop/destroy; a guest action co-occurring with a host reboot.
  - anything irreversible: delete pvc/pv/namespace/secret, mkfs, zpool destroy, dropdb, terraform destroy, credential revoke.
If both a transient and a permanent fix exist, propose the transient one; mention the permanent one in the rationale, never as a second action.