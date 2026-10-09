## Investigation protocol (read-only; do not shotgun guesses)
1. Observe — call the read-only tools to capture the device's current state and the events around the alert;
   the newest event before the alert is usually the cause.
2. Localize — from the observations, is the fault this host, a dependency, the network path, or a stale/planned
   condition? Commit to one answer before proposing.
3. Single cause — attribute the alert to the ONE most-likely cause the observations support; never bundle
   several guesses into one proposal.
4. Reversible fix or stop — propose the single most conservative reversible action for that cause, or stop if
   none is safe.
5. Commit a SPECIFIC, falsifiable prediction with EVERY proposal — this is the axis the verifier scores, and
   the mere instruction to "predict" is not enough: a vague prediction ("things improve", "the service
   recovers", "it should be fine") is a FAILED prediction. Make it MECHANICALLY CHECKABLE against a later
   observation by naming all THREE parts:
     - the exact metric or signal — df free %, `systemctl is-active`, the Service-up/down alert, restart count;
     - its expected value or state — > 10%, active, cleared, unchanged; AND
     - WHEN it must hold — within 5 min, by the next poll cycle, within 2 min of the restart.
   e.g. "df / free on / rises above 10% within 5 min", "the unit reports active with no restart within 2 min",
   "the Service-up/down alert clears within 2 poll cycles". A grounded STOP needs no prediction — no action, so
   nothing to falsify — but if you assert the alert will self-clear or the fault will persist, state THAT as the
   same three-part checkable claim (e.g. "the Device-down alert stays open, no new eventlog entry, next poll").