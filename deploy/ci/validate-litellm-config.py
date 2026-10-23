#!/usr/bin/env python3
"""CI validator for the LiteLLM model-gateway config (deploy/litellm-config.yaml).

The gateway config is the model ladder TG's whole reasoning plane runs on. LiteLLM tolerates a partially
broken config — it can skip a malformed model entry and boot anyway — so a typo'd model_name or a
router fallback that points at an UNDEFINED model silently breaks the failover ladder while the container
stays UP (no crash, no obvious error). This gate catches those at merge time:

  1. the YAML parses;
  2. model_list is a non-empty list, and every entry has a string model_name + a litellm_params dict with a
     'model' key;
  3. every model name referenced by router_settings.fallbacks AND router_settings.content_policy_fallbacks
     (both the keyed model AND each fallback target) is actually DEFINED in model_list — so a fallback can
     never dangle. content_policy_fallbacks was added 2026-08-31 (TG-556 review): that block is this file's
     recurring defect surface (ROUND 2/3/4 all edited it) and had shipped unvalidated three times.

Pure stdlib except PyYAML; no network. Commented-out (embedding) blocks are ignored — only the active config.
"""
import os
import sys

import yaml

CFG = os.path.join(os.path.dirname(__file__), "..", "litellm-config.yaml")


def main():
    try:
        cfg = yaml.safe_load(open(CFG))
    except Exception as e:
        print("FAIL: litellm-config.yaml is not valid YAML:", e)
        return 1

    errs = []
    models = cfg.get("model_list") or []
    if not isinstance(models, list) or not models:
        print("FAIL: model_list is missing or empty")
        return 1

    names = set()
    for i, m in enumerate(models):
        nm = m.get("model_name")
        if not isinstance(nm, str) or not nm:
            errs.append("model_list[%d]: missing/invalid model_name" % i)
            continue
        names.add(nm)
        lp = m.get("litellm_params")
        if not isinstance(lp, dict) or not lp.get("model"):
            errs.append("model %r: missing litellm_params.model" % nm)

    rs = cfg.get("router_settings") or {}
    nrules = 0
    for chain_key in ("fallbacks", "content_policy_fallbacks"):
        for fb in rs.get(chain_key) or []:
            if not isinstance(fb, dict):
                errs.append("router_settings.%s: entry is not a mapping: %r" % (chain_key, fb))
                continue
            nrules += 1
            for keyed, targets in fb.items():
                if keyed not in names:
                    errs.append("%s keyed on undefined model %r" % (chain_key, keyed))
                for t in targets or []:
                    if t not in names:
                        errs.append("%s %r -> undefined model %r" % (chain_key, keyed, t))

    for e in errs:
        print("FAIL", e)
    if errs:
        return 1
    print("OK: %d models (%s); %d fallback rule(s) across fallbacks+content_policy_fallbacks, all references resolve" % (
        len(names), ", ".join(sorted(names)), nrules))
    return 0


if __name__ == "__main__":
    sys.exit(main())
