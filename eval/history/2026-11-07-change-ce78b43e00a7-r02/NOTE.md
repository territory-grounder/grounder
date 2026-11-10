Second full-rigor run of the SAME candidate code as `2026-11-07-change-ce78b43e00a7` (the TG-663 change, code commit
ce78b43e). The archive was written while HEAD was e57c5647 — ce78b43e plus the committed FAIL record and nothing else
(no Go, prompt or registry delta) — so it is filed under the candidate's key as its -r02 (TG-587/TG-591 same-key
policy): run 1 FAIL on falsifiable_prediction alone (−0.31 vs −0.30, overall/recall/controls PASS); run 2 PASS on every
bar (falsifiable_prediction +0.16). The change touches no prompt or reasoning surface; the swing is judge noise on a
3-run arm, recorded as what it is.
