#!/usr/bin/env python3
"""Tests for pair alignment (P6-1) — runnable directly, like its siblings in this directory.

A PAIR IS THE UNIT OF THE ENTIRE CAMPAIGN. If two sides of a "pair" were looking at different incidents, the
comparison is not a comparison, and no amount of downstream statistical care recovers it. These tests exist
because the previous rule (same host, |dt| <= 12h, first match wins) could and would do exactly that.
"""
import os
import sys

os.environ.setdefault("WORK", "/tmp")
os.environ.setdefault("SB_DIR", os.path.dirname(os.path.abspath(__file__)))
os.environ.setdefault("SCORECARD", "/tmp/_test_scorecard.jsonl")
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import _driver as d  # noqa: E402

FAILURES = []


def check(cond, msg):
    if cond:
        print(f"  ok: {msg}")
    else:
        print(f"  FAIL: {msg}")
        FAILURES.append(msg)


# Every alert rule OBSERVED LIVE on this estate, as a closed enumeration. An unclassified rule does not fail
# loudly — it silently removes that entire fault stratum from the campaign, which is the worst kind of gap
# because the resulting report still looks complete.
LIVE_RULES = {
    "Space-on-/-is-90-and-95-in-use": "disk",
    "Device-Down": "device",
    "Devices-up/down": "device",
    "Device-Down-Due-to-no-ICMP-response.": "device",
    "Device-Down-SNMP-unreachable": "device",
    "Device-rebooted": "device",
    "Service-up/down": "service",
    "TG-84-guinea-pig-High-Memory-85-os-agnostic": "memory",
    "Linux High Memory Usage": "memory",
}


def test_every_live_rule_classifies():
    print("every live alert rule classifies into its stratum")
    for rule, want in LIVE_RULES.items():
        got = d.fault_class(rule)
        check(got == want, f"{rule!r} -> {got!r} (want {want!r})")


def test_separator_normalisation():
    print("hyphenated and spaced spellings agree")
    # The bug this pins: patterns were written spaced, real rule names are hyphenated, so EVERY disk alert
    # classified as unknown and the disk stratum would have vanished silently.
    check(d.fault_class("Space-on-/-is-90-and-95-in-use") == d.fault_class("disk space"), "disk both spellings")
    check(d.fault_class("Device-Down") == d.fault_class("device down"), "device both spellings")


def test_unknown_text_is_unclassified_not_guessed():
    print("an unrecognised rule yields no class rather than a guess")
    check(d.fault_class("Some-Novel-Rule") == "", "novel rule -> ''")
    check(d.fault_class("") == "", "empty -> ''")
    check(d.fault_class(None) == "", "None -> ''")


def test_cross_class_pairing_is_refused():
    print("a pair requires the SAME fault class on both sides")
    pred = [{"host": "h1", "firstTs": "2026-07-27T12:00:00+00:00", "alertCategory": "disk space", "incidentKey": "k"}]
    tg = [{"host": "h1", "alertRule": "Device-Down", "createdAt": "2026-07-27T12:01:00+00:00"}]
    pairs, pred_only, _ = d.align(pred, tg)
    check(len(pairs) == 0, "a disk incident does not pair with a device incident one minute away")
    check(len(pred_only) == 1, "the unpaired predecessor incident is reported, not dropped silently")


def test_right_class_beats_nearer_wrong_class():
    print("class match dominates time proximity")
    pred = [{"host": "h1", "firstTs": "2026-07-27T12:00:00+00:00", "alertCategory": "disk space", "incidentKey": "k"}]
    tg = [
        {"host": "h1", "alertRule": "Device-Down", "createdAt": "2026-07-27T12:01:00+00:00"},
        {"host": "h1", "alertRule": "Space-on-/-is-90-and-95-in-use", "createdAt": "2026-07-27T13:30:00+00:00"},
    ]
    pairs, _, _ = d.align(pred, tg)
    check(len(pairs) == 1, "exactly one pair")
    check(
        pairs and pairs[0][1]["alertRule"].startswith("Space-on"),
        "picked the right-class row 90 min away over the wrong-class row 1 min away",
    )


def test_nearest_in_time_wins_within_a_class():
    print("among same-class candidates the NEAREST in time is chosen (deterministic, order-independent)")
    pred = [{"host": "h1", "firstTs": "2026-07-27T12:00:00+00:00", "alertCategory": "disk space", "incidentKey": "k"}]
    far = {"host": "h1", "alertRule": "Space-on-/-is-90-and-95-in-use", "createdAt": "2026-07-27T15:00:00+00:00"}
    near = {"host": "h1", "alertRule": "Space-on-/-is-90-and-95-in-use", "createdAt": "2026-07-27T12:10:00+00:00"}
    a, _, _ = d.align(pred, [far, near])
    b, _, _ = d.align(pred, [near, far])
    check(a[0][1]["createdAt"] == near["createdAt"], "nearest chosen when the far row comes first")
    check(
        a[0][1]["createdAt"] == b[0][1]["createdAt"],
        "the SAME pair regardless of row order — the old first-match rule made this order-dependent, which "
        "is disqualifying for a campaign that must reproduce to the digit",
    )


def test_a_tg_row_is_never_used_twice():
    print("one TG session cannot serve two predecessor incidents")
    pred = [
        {"host": "h1", "firstTs": "2026-07-27T12:00:00+00:00", "alertCategory": "disk space", "incidentKey": "k1"},
        {"host": "h1", "firstTs": "2026-07-27T12:05:00+00:00", "alertCategory": "disk space", "incidentKey": "k2"},
    ]
    tg = [{"host": "h1", "alertRule": "Space-on-/-is-90-and-95-in-use", "createdAt": "2026-07-27T12:01:00+00:00"}]
    pairs, pred_only, _ = d.align(pred, tg)
    check(len(pairs) == 1, "only one pair is formed")
    check(len(pred_only) == 1, "the second incident is reported unpaired rather than reusing the same session")



# THE PREDECESSOR'S VOCABULARY IS ITS OWN, AND IT IS NOT A FAULT CLASS.
#
# Every predecessor record populates alert_category from availability / maintenance / general / kubernetes /
# resource. Classifying on the FIRST non-empty field meant alertCategory always won and always returned "" —
# so the fault-class requirement refused 100% of pairs, silently, because an unclassifiable side is designed
# to yield no pair rather than error. The campaign would have accrued NOTHING while the harness printed
# "no aligned pairs yet", which reads exactly like a quiet estate.
#
# These are REAL titles taken from the live gateway.db.
LIVE_PREDECESSOR_INCIDENTS = [
    ("Flapping infra alert: Device Down! Due to no ICMP response. on dc1wallos01", "availability", "device"),
    ("Infrastructure alert: dc1openwebui01 - Space on / is >= 90% and < 95% in use", "general", "disk"),
    ("Infrastructure alert: dc1ghostfolio01 - Service up/down. Escalation reason: s", "availability", "service"),
    ("Flapping infra alert: Devices up/down on dc1cloudbeaver01 (flap #9)", "availability", "device"),
    ("Infrastructure alert: dc1x - Linux High Memory Usage", "resource", "memory"),
]


def test_real_predecessor_incidents_classify():
    print("every REAL predecessor incident classifies (its category alone never does)")
    for title, cat, want in LIVE_PREDECESSOR_INCIDENTS:
        got = d.fault_class(" ".join([title, "", cat]))
        check(got == want, f"[{cat}] {title[:52]!r} -> {got!r} (want {want!r})")


def test_the_coarse_category_alone_is_not_enough():
    print("the classifier must read the TITLE, not stop at the category")
    # "general" and "maintenance" name no fault class at all. If the classifier only saw the category, a disk
    # incident labelled "general" would be unclassifiable and its pair silently refused.
    check(d.fault_class("general") == "", "'general' alone classifies to nothing")
    check(d.fault_class("maintenance") == "", "'maintenance' alone classifies to nothing")
    check(
        d.fault_class("Infrastructure alert: h - Space on / is >= 90% general") == "disk",
        "a disk title labelled 'general' still classifies as disk",
    )


def test_a_specific_rule_name_beats_the_coarse_category():
    print("a specific rule name wins over the coarse hint")
    # 'availability' hints device; the title says Service up/down. The title must win, or every service
    # incident would be mis-paired against a device one — the exact cross-class pairing this rule prevents.
    got = d.fault_class("Infrastructure alert: h - Service up/down availability")
    check(got == "service", f"Service up/down under an 'availability' category -> {got!r} (want 'service')")


def test_pair_records_carry_the_tier_tag():
    print("appended records carry the additive tier tag (TG-72)")
    # Default is "1": every record written before the field existed is a tier-1-era record, so absent ⇒ "1"
    # and old scorecards stay valid. The tag steers tier-scoped SELECTION only — analyze.py is sha-frozen
    # and ignores unknown fields — so this asserts the writer, not the analysis.
    import importlib
    import tempfile

    with tempfile.NamedTemporaryFile("r", suffix=".jsonl") as fh:
        os.environ["SCORECARD"] = fh.name
        os.environ.pop("SB_TIER", None)
        importlib.reload(d)
        d.append_verdict({}, "k1", "pair")
        os.environ["SB_TIER"] = "2"
        importlib.reload(d)
        d.append_verdict({}, "k2", "pair")
        d.append_verdict({"tier": "7"}, "k3", "pair")   # an explicit tier is never overwritten
        import json as _json
        recs = [_json.loads(ln) for ln in open(fh.name) if ln.strip()]
    check([r.get("tier") for r in recs] == ["1", "2", "7"],
          f"tier tags default '1', honor SB_TIER, keep explicit values (got {[r.get('tier') for r in recs]})")
    os.environ.pop("SB_TIER", None)
    os.environ["SCORECARD"] = "/tmp/_test_scorecard.jsonl"
    importlib.reload(d)


for fn in (
    test_every_live_rule_classifies,
    test_separator_normalisation,
    test_unknown_text_is_unclassified_not_guessed,
    test_cross_class_pairing_is_refused,
    test_right_class_beats_nearer_wrong_class,
    test_nearest_in_time_wins_within_a_class,
    test_a_tg_row_is_never_used_twice,
    test_real_predecessor_incidents_classify,
    test_the_coarse_category_alone_is_not_enough,
    test_a_specific_rule_name_beats_the_coarse_category,
    test_pair_records_carry_the_tier_tag,
):
    fn()

print()
if FAILURES:
    print(f"pair-alignment tests: FAIL ({len(FAILURES)})")
    sys.exit(1)
print("pair-alignment tests: PASS")
