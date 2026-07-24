#!/usr/bin/env python3
"""Assemble index.html = console.html + fonts + charts foundation + surface modules + glue.
Keeps console.html canonical (markers intact); rebuild is idempotent AND byte-reproduces the
DEPLOYED deploy/console/v2/index.html (guarded by `make console-verify` / the CI console-drift job).
See #55: the deployed index.html is the served artifact (the Dockerfile overwrites the built React
index with it), so this pipeline is its source of truth — assembling MUST NOT change index.html.

Module placement mirrors the deployed file exactly (do not "tidy" the ordering — it changes bytes):
  * CSS: charts + CSS_EARLY at /*EXT_CSS*/ (skills+wiki sit right after secrets in the stylesheet);
    the core `responsive` block follows; then CSS_LATE (credentials/policy/regime) at /*EXT_CSS_LATE*/.
  * JS : charts + JS_MODS + glue + the live layer, then LATE_JS (skills+wiki) appended AFTER the
    live layer."""
import os, re, sys
os.chdir(os.path.dirname(os.path.abspath(__file__)))

CSS_EARLY = ["workflows", "logs", "signals", "estatedepth", "grounding", "knowledge", "models", "modules", "secrets", "skills", "wiki"]
CSS_LATE  = ["credentials", "policy", "regime"]
JS_MODS   = ["workflows", "logs", "signals", "estatedepth", "grounding", "knowledge", "models", "modules", "secrets", "credentials", "policy", "regime"]
LATE_JS   = ["skills", "wiki"]

def read(p):
    return open(p).read() if os.path.exists(p) else ""

def emit_module(m):
    """One module's JS: fixtures + view + wiring, headered. Nav is wired statically in console.html,
    so module-side nav-registration lines are stripped."""
    fix, js, wiring = read(f"modules/{m}/fixtures.txt"), read(f"modules/{m}/js.txt"), read(f"modules/{m}/wiring.txt")
    wiring = "\n".join(l for l in wiring.splitlines() if "AddNav(" not in l and "addNav(" not in l)
    return [f"/* ==== module: {m} — fixtures ==== */", fix,
            f"/* ==== module: {m} — view ==== */", js,
            f"/* ==== module: {m} — wiring ==== */", wiring]

html = read("console.html")
fonts = read("fonts.css")

# ---- CSS bundle: charts + early modules at /*EXT_CSS*/; late modules at /*EXT_CSS_LATE*/ ----
css_parts = ["\n/* ==== charts foundation ==== */\n" + read("modules/_charts/css.txt")]
for m in CSS_EARLY:
    css_parts.append(f"\n/* ==== module: {m} ==== */\n" + read(f"modules/{m}/css.txt"))
ext_css = "\n".join(css_parts)
# late CSS group — injected after the core `responsive` block (see console.html /*EXT_CSS_LATE*/)
ext_css_late = "\n".join(f"\n/* ==== module: {m} ==== */\n" + read(f"modules/{m}/css.txt") for m in CSS_LATE)

# ---- JS bundle ----
js_parts = []
js_parts.append("/* ==== charts foundation (exports svgEl, sparkline, areaChart, metricPanel, smallMultiples, donut, hbar, mulberry32) ==== */")
js_parts.append(read("modules/_charts/js.txt"))
js_parts.append("/* charts fixtures/generators */")
js_parts.append(read("modules/_charts/fixtures.txt"))
# safety shim if the foundation failed to export
js_parts.append("""
if (typeof window.svgEl !== "function") {
  window.svgEl = function(tag, attrs={}, ...kids){
    const e=document.createElementNS("http://www.w3.org/2000/svg",tag);
    for(const[k,v]of Object.entries(attrs)){ if(v==null||v===false)continue;
      if(k==="html")e.innerHTML=v; else if(k.startsWith("on")&&typeof v==="function")e.addEventListener(k.slice(2),v);
      else e.setAttribute(k,v===true?"":v); }
    for(const k of kids.flat(Infinity)){ if(k==null||k===false)continue; e.append(k.nodeType?k:document.createTextNode(k)); }
    return e; };
}
""")
for m in JS_MODS:
    js_parts += emit_module(m)

# ---- glue: badges + sessions alias ----
js_parts.append("""
/* ==== glue: badges + aliases ==== */
if (typeof wfView === "function") { views.sessions = wfView; }  /* old #sessions hash keeps working */
if (typeof FACETS !== "undefined" && FACETS.workflows && !facetState.workflows) facetState.workflows = "all"; /* default active facet, like every other view */
(function(){
  const set=(k,v)=>{$$('[data-badge="'+k+'"]').forEach(el=>el.textContent=String(v));};
  try{ if(typeof WF_RUNS!=="undefined") set("wf_live", WF_RUNS.filter(r=>r.live).length); }catch(e){}
  try{ if(typeof LOG_LINES!=="undefined") set("logs", LOG_LINES.length); }catch(e){}
  try{ if(typeof ESTATE!=="undefined") set("estatedepth", ESTATE.nodes.filter(n=>n.health!=="ok").length); }catch(e){}
  try{ if(typeof MDL_USAGE!=="undefined"&&MDL_USAGE["24h"]) set("models", MDL_USAGE["24h"].fallbackEvents); }catch(e){}
  try{ if(typeof MODS_FLEET!=="undefined") set("modules", MODS_FLEET.filter(m=>m.enabled).length+"/"+MODS_FLEET.length); }catch(e){}
  try{ if(typeof SEC_REFS!=="undefined") set("secretsMissing", SEC_REFS.filter(s=>String(s.status).toLowerCase()==="missing").length); }catch(e){}
  try{ if(typeof GROUNDING!=="undefined"&&GROUNDING.match_rate!=null) set("grounding", Math.round(GROUNDING.match_rate*100)+"%"); }catch(e){}
  try{ if(typeof KNOW!=="undefined") set("knowledge", (KNOW.hosts||[]).length+(KNOW.incidents||[]).length+(KNOW.rules||[]).length); }catch(e){}
  try{ if(typeof sigTILES!=="undefined"){ const a=sigTILES.find(t=>/anomal/i.test(t.lbl)); if(a){ const n=String(a.val).match(/\d+/); if(n) set("signals", n[0]); } } }catch(e){}
})();
""")
# ---- live layer: the real API client (login + posture/ledger/sessions adoption) ----
js_parts.append(read("modules/_live/js.txt"))
# ---- late JS surfaces: skills + wiki are appended AFTER the live layer (matches the deployed file);
#      their eager-live-load wiring wraps liveAdopt(), which is defined by the live layer above ----
for m in LATE_JS:
    js_parts += emit_module(m)

ext_js = "\n".join(js_parts)

assert all(k in html for k in ("/*FONTS*/", "/*EXT_CSS*/", "/*EXT_CSS_LATE*/", "/*EXT_JS*/")), "markers missing"
out = (html.replace("/*FONTS*/", fonts, 1)
           .replace("/*EXT_CSS*/", ext_css, 1)
           .replace("/*EXT_CSS_LATE*/", ext_css_late, 1)
           .replace("/*EXT_JS*/", ext_js, 1))
assert "</script" not in ext_js, "script-close hazard in bundle"

# `--check` (used by `make console-verify` / the CI console-drift job): assemble in-memory and FAIL if
# it does not byte-match the committed index.html — the served artifact must stay the source's output.
if "--check" in sys.argv:
    have = read("index.html")
    if have != out:
        n = min(len(have), len(out)); i = next((k for k in range(n) if have[k] != out[k]), n)
        sys.stderr.write(
            "console drift: assemble.py output does NOT match the committed deploy/console/v2/index.html\n"
            f"  committed={len(have)}B  assembled={len(out)}B  first diff @ char {i}\n"
            "  index.html is a BUILD ARTIFACT of this pipeline (see #55). Fix by editing console.html /\n"
            "  modules/* and re-running `python3 assemble.py`; never hand-edit index.html.\n")
        sys.exit(1)
    print(f"console-verify OK: assemble.py reproduces index.html byte-for-byte ({len(out)} bytes)")
    sys.exit(0)

open("index.html", "w").write(out)
print(f"index.html: {len(out)} bytes  (css {len(ext_css)}+{len(ext_css_late)} late, js bundle {len(ext_js)})")
