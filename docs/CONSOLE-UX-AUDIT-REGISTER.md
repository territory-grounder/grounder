> **NOT A WORK QUEUE — reference only.** The one authoritative queue is
> [`BOARD.md`](BOARD.md): work that, top-down. Anything below that reads as a priority, a ranking, a
> "first step" or a "do this next" is a RECORD OF WHAT WAS THOUGHT AT THE TIME and does not steer.
> The complete inventory of open work is YouTrack `project: TG #Unresolved` — the board is the ranked
> queue, not the inventory, and its silence is not closure.
>
> *Why this banner exists: on 2026-08-02 seventeen files in this repo told a reader what to do next. One
> of them had listed "Wire `VoteAdmission`" as a named GATE; that file was later quarantined for being
> unreadable and the gate went with it, leaving the approval control enforced by nothing from July to
> August (TG-254). A second authority is how a finding gets lost.*

# Console UX audit register — CLOSED PERMANENTLY (2026-07-30)

> **This register is closed and the agent-driven audit-sweep method is retired** (recovery Phase
> E1, owner-approved). Seven sweeps by ~180 agents produced 149 candidates with a 23.5%
> false-finding rate, declared closure three times, and were followed by more defects each time —
> the method samples where the defect class demands enumeration, and its findings were repeatedly
> "fixed" against oracles that could not see the production path. Console defects now ride the
> normal bug flow: a report, a failing e2e oracle against the REAL endpoint first, then the fix.
> Do not launch another sweep; do not reopen this register.

_Generated 2026-07-29 from the audit workflow journals. This file is the DURABLE record — the
workflow journals are session-scoped and will be lost._

## Why this file exists

Four audit sweeps were run before this register was written, because the dimension space was never
enumerated up front: each sweep's lenses were chosen from memory, gaps were discovered afterwards,
and another sweep was launched to cover them. That is *sampling where enumeration was required* —
the same defect class the audits themselves kept finding in the console. The enumeration below is
derived from WCAG 2.2 (Level A + AA) plus a named set of operator-console dimensions, so coverage
is now a checklist with gaps that can be pointed at, not a feeling.

**Rule going forward: no further broad sweep.** Only the rows marked UNCOVERED may be audited, and
when one is, its row is updated here in the same change.


## Totals

- sweeps: 4 · lenses: 19 · agents: ~90
- findings: **66 confirmed**, **25 refuted** (a 28% refutation rate; the dominant
  failure mode was a finding whose measurement reproduced and whose consequence did not)
- WCAG 2.2 A+AA rows: 51 — covered 37, **UNCOVERED 15**


## WCAG 2.2 (A + AA) coverage

| SC | Name | Level | Covered by |
|---|---|---|---|
| 1.1.1 | Non-text Content | A | S1 controls / S2 AT-tree |
| 1.2.x | Time-based media | A/AA | N/A — the console has no audio or video |
| 1.3.1 | Info and Relationships | A | S1 forms, S2 AT-tree, tables/th-scope work |
| 1.3.2 | Meaningful Sequence | A | S2 AT-tree (heading outline per view) |
| 1.3.3 | Sensory Characteristics | A | S2 colour-alone |
| 1.3.4 | Orientation | AA | **UNCOVERED** |
| 1.3.5 | Identify Input Purpose | AA | S1 forms (autocomplete on credential fields) |
| 1.4.1 | Use of Color | A | S2 colour-alone (Machado dichromacy matrices) + forced-colors |
| 1.4.2 | Audio Control | A | N/A — no audio |
| 1.4.3 | Contrast (Minimum) | AA | contrast-tokens oracle, both themes, all 22 views + tints |
| 1.4.4 | Resize Text | AA | S2 rtl-i18n (200% root font) |
| 1.4.5 | Images of Text | AA | **UNCOVERED** |
| 1.4.10 | Reflow | AA | S1 responsive + KILL sweep 1024-1920 + 390px |
| 1.4.11 | Non-text Contrast | AA | UNCOVERED — token work covered TEXT contrast only |
| 1.4.12 | Text Spacing | AA | S2 rtl-i18n (1.4.12 override block) |
| 1.4.13 | Content on Hover or Focus | AA | **UNCOVERED** |
| 2.1.1 | Keyboard | A | S1 keyboard + #workflows zero-focusables fix |
| 2.1.2 | No Keyboard Trap | A | S1 overlays + overlay-semantics oracle |
| 2.1.4 | Character Key Shortcuts | A | **UNCOVERED** |
| 2.2.1 | Timing Adjustable | A | N/A — no time limits on interaction |
| 2.2.2 | Pause, Stop, Hide | A | S3 jank (animation under reduced-motion) |
| 2.3.1 | Three Flashes | A | S3 jank (no flashing content observed) |
| 2.4.1 | Bypass Blocks | A | skip-link work (!719) |
| 2.4.2 | Page Titled | A | per-view document.title work (!719) |
| 2.4.3 | Focus Order | A | S1 keyboard + overlay focus-return work |
| 2.4.4 | Link Purpose (In Context) | A | S2 AT-tree |
| 2.4.5 | Multiple Ways | AA | **UNCOVERED** |
| 2.4.6 | Headings and Labels | AA | S1 forms + table-naming work |
| 2.4.7 | Focus Visible | AA | S1 keyboard (focus indicator measured) |
| 2.4.11 | Focus Not Obscured (Min) | AA | **UNCOVERED** |
| 2.5.1 | Pointer Gestures | A | S2 touch-print |
| 2.5.2 | Pointer Cancellation | A | **UNCOVERED** |
| 2.5.3 | Label in Name | A | **UNCOVERED** |
| 2.5.4 | Motion Actuation | A | N/A — no motion-actuated controls |
| 2.5.7 | Dragging Movements | AA | **UNCOVERED** |
| 2.5.8 | Target Size (Minimum) | AA | S1 controls (spacing exception computed) + e2-navh fix |
| 3.1.1 | Language of Page | A | html[lang] work (!719) |
| 3.1.2 | Language of Parts | AA | N/A — single-language console |
| 3.2.1 | On Focus | A | **UNCOVERED** |
| 3.2.2 | On Input | A | **UNCOVERED** |
| 3.2.3 | Consistent Navigation | AA | S1 nav-switch oracle |
| 3.2.4 | Consistent Identification | AA | S1 copy lens |
| 3.2.6 | Consistent Help | A | **UNCOVERED** |
| 3.3.1 | Error Identification | A | S1 forms + seal-form validation work |
| 3.3.2 | Labels or Instructions | A | S1 forms + placeholder-name work |
| 3.3.3 | Error Suggestion | AA | seal-form validation work |
| 3.3.4 | Error Prevention (Legal/Financial/Data) | AA | **UNCOVERED** — and the highest-stakes row here: Approve/Deny commits a real actuation |
| 3.3.7 | Redundant Entry | A | **UNCOVERED** |
| 3.3.8 | Accessible Authentication (Min) | AA | **UNCOVERED** |
| 4.1.2 | Name, Role, Value | A | S1 controls + S2 AT-tree + keyboard work |
| 4.1.3 | Status Messages | AA | announcements oracle (!726) |

## Non-WCAG operator-console dimensions

| Dimension | Question | Covered by |
|---|---|---|
| DATA-HONESTY | Does a surface ever assert something untrue | S1 honesty + fabrication work (#69) |
| FRESHNESS | Is displayed data current, and is staleness visible | S3 jank -> live-refresh work (!728) |
| FAILURE-STATES | refused vs broken vs absent vs empty | S1 states + fabrication work |
| PRINT | What an operator gets in a post-mortem PDF | S2 touch-print -> print stylesheet (!727) |
| FORCED-COLORS | High-contrast / Windows HCM | S2 forced-colors |
| RTL | Right-to-left mirroring | S2 rtl-i18n |
| MULTI-TAB | Two tabs, one operator | S4 multi-tab |
| SESSION-BOUNDARY | Sign-out / expiry seen from another tab | S4 multi-tab |
| SR-SPEECH | What is actually ANNOUNCED, not the tree | S4 sr-speech |
| INFO-DESIGN | Could a correct-looking screen cause a wrong decision | S1 copy + S4 information-design |
| STABILITY | Layout shift, long tasks, DOM growth | S3 jank |
| CAPABILITY | Can the operator actually ACT (controls present at all) | S1 -> uncalled approvals strip (!730) |

## Still UNCOVERED — the ONLY legitimate scope for further auditing

> **2026-07-29 — THIS LIST IS NOW EMPTY.** The 15 rows that stood here were audited as workflow
> `ux-uncovered-15-final` (13 PASS · 1 confirmed · 1 refuted, § S6 below), and the three lenses recorded as
> never run — real screen-reader speech, multi-tab/concurrent session, session-boundary flows — were
> executed as `ux-final-lenses` (§ S5 below). No further broad sweep is authorised. Any new audit must add
> its dimension as a row here BEFORE it runs, and update that row in the same change.

## Confirmed findings (66)

1. `[S1/medium]` One line, mirrored in the two bundles. In `/home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/modules/_live/js.txt:969` (and the generated copy at `index.html:8181`), give the error div the
2. `[S1/medium]` In cfgSealForm() at /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html:8266, the gate() closure already has everything it needs and the msg div already exists with reserved heigh
3. `[S1/low]` Add an aria-label to each of the 10 controls — the same one-attribute pattern the console already uses on input.wk-search and select.pol-lim. In deploy/console/v2/index.html: cfgSealForm() (~L8256-8262) ni/vi/pi/ra -> "s
4. `[S1/high]` Delete the hard-coded literal and derive the sentence from live posture, reusing the honesty rule `setBreaker()` already implements at /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/ind
5. `[S1/high]` In /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html, keep null (read failed) distinguishable from [] (read succeeded, empty) in the #logs view, the same way the alerts view alr
6. `[S1/high]` In deploy/console/v2/index.html, make the failed read visible instead of silent. (a) At the ledger catch (line ~7422) replace `catch(e){ /* ledger stays fixture-labeled */ }` with `catch(e){ liveState.ledger = null; live
7. `[S1/medium]` Distinguish "never fetched" from "fetch failed" — the honest copy already exists, it just is not reachable from the error path. 1. deploy/console/v2/index.html:7474 — record the failure: `try{ liveState.estate = await li
8. `[S1/medium]` One base-level CSS rule, not five per-class rules. The console already owns a placeholder colour that passes — the token behind `.know-search` / `.wk-search`, which resolves to rgb(146,164,172) in dark (6.26:1) and rgb(8
9. `[S1/medium]` Two changes; the first is the one that makes the surface honest, the second shrinks the window. 1. Generalise the pre-resolve state that `views.alerts` already implements. `liveState.<key>` is `undefined` before its read
10. `[S1/low]` One line in the account-menu IIFE of /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/console.html (canonical; ~line 1670, right after `const close=...`), then rebuild the served artifact
11. `[S1/high]` One shared helper in /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html, called from all four open/close pairs — openDrawer (~line 2724) / closeDrawer (line 2767), modsOpenDrawer
12. `[S1/high]` Two lines in `closeDrawer()` plus one in `openSession()`, in /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/console.html. `inert` alone removes every leftover tab stop (4 or 9) and is w
13. `[S1/medium]` Confine the change to renderPal/paint and the palette markup (index.html:2101-2108 and 2851-2874; mirror into console.html:835-840, 1581-1606). 1. Markup: `<input id="palInput" role="combobox" aria-expanded="false" aria-
14. `[S1/medium]` In cfgElevateOverlay() (live bundle ~line 8153), hoist the duplicated `const el=$("#cfgElevate");el&&el.remove();` into one closeElev() and wire the two missing dismissals to it before/after `document.body.append(scrim)`
15. `[S1/high]` Two lines on the deployed artifact, both already prototyped in the dirty working tree — they just need committing and deploying: 1. Stop asserting a compile-time posture. `setBreaker()` is the only function that ever lea
16. `[S1/medium]` In /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/console.html, in openSession(), mirror what modsOpenDrawer() already does correctly. Change the open block from: d.append(b); $("#scrim
17. `[S1/high]` The console already learns the posture in exactly one place — `setBreaker(mutation, stale, source)`. Make that the only source and delete the seven literals. Have `setBreaker` record the posture, and add a `postureClaim(
18. `[S1/high]` Smallest correct fix: delete both literals and render the posture the console already receives. `setBreaker(mutation, stale, source)` is the only code that learns the real posture (it is in the deployed bundle and correc
19. `[S1/medium]` Smallest correct fix: `apiView()` already has the parsed method list in scope as `paths[pth]` — just pass it in, so the label stops being a constant. In /home/tg/gitlab/products/territory-grounder/grounder/dep
20. `[S1/medium]` Two edits in /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html, both inside grndRender(): 1. Line ~5172 — stop prefixing "×" to a floored, non-ratio quantity, and put the two re
21. `[S1/medium]` Two coupled changes — the ratio alone is NOT sufficient and shipping it alone would assert a false positive. 1. /home/tg/gitlab/products/territory-grounder/grounder/cmd/grounder/deps.go:288-292 — honour the co
22. `[S1/medium]` One line, at /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html:8912. Copy the pattern the alerts view already uses at index.html:7728-7730, reusing the two values the logs badge
23. `[S1/medium]` Two-line string change plus its oracle. In /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html line 4813 and line 4999 (and the mirrored copy in deploy/console/v2/modules/estatede
24. `[S1/high]` Two literals must become reads of the posture the console already has. Both are mirrored in the split modules, so fix all four sites or the assembler reintroduces them. 1. The head chip — deploy/console/v2/modules/workfl
25. `[S1/medium]` Make the emptiness explanation derive from the live posture instead of hardcoding "Shadow". Five string sites in /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html: 1. Line 7077 
26. `[S1/high]` Two changes, in priority order. 1. WIRE THE STRIP (the whole severity). liveApprovalsStrip() is defined at live-bundle line 7803 and never called. Compose it into wfView above the decision-tracer inspector - the source c
27. `[S1/high]` One shared helper on the four open/close paths, using `inert` — which removes the background from the tab order AND the accessibility tree in a single attribute, fixing both halves of the finding at once. On open: `docum
28. `[S1/medium]` One line, at two mirrored sites (the modules/*.txt source and its index.html mirror must move in lockstep): /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/modules/workflows/js.txt:106 /
29. `[S1/medium]` Smallest correct fix, in `estateNodeDetail` at /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html:4813-4820 (and the matching module source under deploy/console/v2/modules/): sto
30. `[S1/medium]` One line of text in the `.sec` header of the #command sessions block. The view already receives `total` in the /api/v1/sessions?limit=50 payload (it is discarded today), and the exact sentence already ships in #knowledge
31. `[S1/medium]` Two-line fix, both in the live path (deploy/console/v2/index.html): 1. Seed the value from data already in hand — the `/v1/sessions` list row carries `classified_at` (verified present on all 50 rows), so in dtoToWfRun re
32. `[S1/medium]` Smallest correct fix, in /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html at the `layers.forEach` render (lines 2556-2565) — put the state into the text run and name the group,
33. `[S1/medium]` One line, in the existing router that already computes the answer. Replace: $$(".navi").forEach(n=>n.classList.toggle("on",n.dataset.view===name)); with: $$(".navi").forEach(n=>{ const cur = n.dataset.view === name; n.cl
34. `[S1/medium]` Convert the existing markup to the ARIA combobox/listbox pattern inside the two functions that already own it — no restructuring needed. In `console.html`: - Static markup (line ~838-839): give `#palInput` `role="combobo
35. `[S1/medium]` Two edits in /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html (and the identical pair in console.html:704 / :1664 if that file is still served). Markup, line 1970 — add the ini
36. `[S1/high]` Three one-line additions in the live bundle's workflows renderer, reusing the keydown idiom the same file already uses at L7338/L7728/L7783. 1. `wfRenderList` (L1711) — the run rows: `h('div',{class:'wf-run' + (r.id === 
37. `[S1/low]` Smallest correct fix — drop the unfulfilled promise rather than build the missing half. In /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html:4687-4689, change the container to `
38. `[S1/medium]` Give the header slack so the emergency stop can never be the overflow victim. Root cause: header.posture is display:flex / flex-wrap:nowrap and its elastic text chips (#annun, #chainChip, .who, .conn) all compute min-wid
39. `[S1/high]` Extend the shrink technique that already exists in the `@media (max-width:820px)` lane (index.html:1784-1826) upward so it covers the 821-1443px band — the header must reach min-content <= viewport at every desktop width
40. `[S1/medium]` Because both overlays live outside #appRoot, one line per open/close contains focus and honours the aria-modal contract — no focus-cycling logic needed: 1. deploy/console/v2/index.html openPal() (~line 2878): after addin
41. `[S1/medium]` At the single render site that toggles the `.on` class on `.facet`, also emit the matching ARIA state — `aria-pressed="true"` on the active chip and `aria-pressed="false"` on the rest — for the 8 true filter bars (comman
42. `[S1/medium]` Two lines, in the canonical source /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/console.html at line 653-654 (the `@media (max-width:820px)` block), then re-run `python3 assemble.py` 
43. `[S1/medium]` In /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/modules/workflows/js.txt (wfRenderList, line ~111), give each row the same keyboard contract the codebase already uses for ribbonRow an
44. `[S1/high]` Two parts; the first alone stops the wedge, the second makes the drawer worth opening. 1) Make openSession un-wedgeable (deploy/console/v2/index.html, mirrored into deploy/console/v2/console.html): - line 2723: `s.action
45. `[S2/medium]` Mirror what the login gate already does. On #cfgAdmErr set role="alert" (or role="status" + aria-live="polite" if assertive is too intrusive) and aria-atomic="true" so every message written at index.html:8338/8343/8344/8
46. `[S2/medium]` Deploy commit 0d84e30 — the fix already exists in /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html (cfgSealForm, ~line 8434) and is simply not on the live host. It replaces the
47. `[S2/high]` Add one app-level polite announcer plus role on the per-form containers, and route the existing 25 message writes through it. 1) In #appRoot (so it survives view switches and is not the pre-auth #agErr), add a visually-h
48. `[S2/medium]` Make the disablement self-explaining at the point of disablement, and stop letting the posture note occupy the explanation slot. In /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/module
49. `[S2/medium]` Two parts, because neither alone is sufficient. 1. Stop the phone breakpoint from claiming paper. In /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/console.html change both responsive b
50. `[S2/medium]` Give a.e2-navh its own vertical padding so the target reaches 24 CSS px without breaking the ellipsis truncation. In /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/modules/estatedepth/c
51. `[S2/medium]` Scope the fix to the eight that survive, and do NOT touch the admin step-up (already correct) or the two #skills rationale textareas (already visibly labelled — adding aria-label there is harmless but is not the defect).
52. `[S2/medium]` Add a print stylesheet to deploy/console/v2/console.html. The exact block I verified live (took #ledger 1->4 pages / 12->41 rows and #actions 1->5 pages / 11->25 rows): @media print { html, body { height: auto !important
53. `[S2/medium]` Give the verified node a channel that is not hue, and put the verdict in text — the bundle already has the pattern at index.html:3902 (`wfVerdictChip`: MATCH ✓ / PARTIAL ◐ / DEVIATION ✕). 1. deploy/console/v2/index.html:
54. `[S2/medium]` Two changes, one of which `main` still does not have. 1. Persistent visible label (the part 0d84e30 does NOT fix). `aria-label` alone leaves every sighted operator with four unlabelled boxes the moment they type. Give ea
55. `[S2/medium]` Root cause, /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html:2879 (mirrored at deploy/console/v2/console.html:1613): function nameTables(root){ for(const t of root.querySelecto
56. `[S2/medium]` Two one-line changes in `spine()` (deploy/console/v2/console.html ~line 999-1020), which is the sole renderer for all 50 rows: 1. Stop the static labels from entering the row's accessible name. The `.caps` spans are posi
57. `[S2/medium]` Root cause is /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html:757-758 — a non-shrinkable, non-truncatable item in a flex row that nothing clips: .wf-rt{display:flex;gap:6px;al
58. `[S2/medium]` The enabling condition is already in place: `<html>` has no explicit font-size and inherits the browser preference (proven — it went 16px→32px under CDP while everything else stayed put). Only the sheet needs to change. 
59. `[S2/medium]` Two changes in /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html. 1) CSS, after line 1092 — give the selected row a cue that forced colors cannot delete. System colours and bord
60. `[S3/high]` Two changes, either of which removes the blind spot; do both. 1. Make the data refresh. Extract the list reads out of liveAdopt() into a `liveRefresh()` (alerts, ledger, sessions, actions, estate) and drive it from (a) a
61. `[S3/low]` No code change — this is a confirmed null result, not a defect. Two edits to the finding text before anyone quotes it: 1. Delete "the only long tasks in the session occur at boot and on view switch." Boot produced 0 long
62. `[S4/medium]` Make the denominator match the contract the field already publishes, and make the suite able to fail on it. 1. cmd/grounder/deps.go:288-292 — stop flooring a non-degenerate control at 1. The only case needing a guard is 
63. `[S4/medium]` Gate the prose on the mode the badge already reads, and pass mode into the empty-state helpers. 1. index.html:7321-7324 — replace the `anyTraffic`-only clause with a three-way that consults `mut`/`stale`: - stale: "; pos
64. `[S4/medium]` In /home/tg/gitlab/products/territory-grounder/grounder/deploy/console/v2/index.html, make `ledgerTime` (line 7707) render UTC like every other timestamp in the console, and give it the date that #command alre
65. `[S4/high]` Stop deriving in-flight state from `a.approved`. In deploy/console/v2/index.html:7686 read the `approval_choice` the DTO already ships: reserve "executing" for `a.executed && !a.verified`; map `approval_choice` of `obsol
66. `[S4/medium]` In modules/_live/js.txt `views.command`, apply the facet to the live rows before building the table, instead of leaving it stranded on the discarded fixture path: const f = (typeof facetState!=="undefined" && facetState.

## Refuted findings (25) — kept so they are not re-raised

1. `[S1]` The finding's attribute-level observations reproduce exactly, but its two load-bearing claims — the headline and the consequence — are both false by direct measurement, and the residual gap does not demonstrably harm an 
2. `[S1]` The raw statistic reproduces exactly, but the operator harm does not: the two statuses the auditor injected (403, 500) cannot be produced on the six/seven views named, and the faithful 401 shows the login gate instead of
3. `[S1]` Both of the auditor's two measured numbers fail to reproduce, and the asymmetry that is the entire substance of the finding does not exist. 1. "Closing with Escape: focus returns to BUTTON.sig-sess — focus-returned-to-tr
4. `[S1]` The finding does not survive. Three of its four load-bearing claims are refuted by live measurement, and its measured state is not reachable by session expiry. 1. **The state is not reachable.** The finding requires `/v1
5. `[S1]` The auditor's quoted strings all reproduce verbatim, but all four load-bearing claims break under measurement. 1. NOT three mutually exclusive statements — two of the three agree, and the third is a different axis. "enab
6. `[S1]` The finding's raw tab count reproduces exactly (29), but every load-bearing claim built on top of it breaks under measurement. (1) SCOPE OVERSTATED BY 12 VIEWS. "all 22 views" is false. Only 10 of 22 views render a facet
7. `[S1]` The finding's CSS premise reproduces exactly, but its operative claim — "no visible focus indicator", "gives no sign that it has focus" — is an artifact of the instrument, and its "only control in the console" superlativ
8. `[S2]` The raw numbers reproduce; the consequence does not. The auditor measured `box.textContent`, which by construction can never contain placeholder text — so their instrument was blind to the one element that states the rul
9. `[S2]` The measurement reproduces exactly; the consequence does not. Three independent refutations. (1) REDUNDANCY IS TOTAL. I swept all three range chips (1h/6h/24h) x 8 delta panels = 24 reachable states. In every one, the go
10. `[S2]` The pixel measurement reproduces perfectly — I confirmed it across a full enumeration of all 22 views, not the auditor's 2-view sample. What fails is the consequence, and specifically the premise that makes it a defect. 
11. `[S2]` REFUTED. I reproduced the geometry, then generated the actual A4 PDFs (Playwright page.pdf, format A4, printBackground true) and read them back with pdftotext and at 300 dpi. Three independent failures: (1) THE LEDGER HA
12. `[S2]` The raw numbers reproduce exactly, but the finding's information claim — and therefore its consequence — is refuted. WHAT I CONFIRMED (identical to the auditor) Light theme, live, #estatedepth, `svg.e2-csvg` (820x266.5 C
13. `[S2]` The raw number reproduces exactly — and the consequence does not. This is precisely the failure mode the brief flags as most common here. WHAT SURVIVED. I re-derived the nine light-theme tokens from the LIVE page (getCom
14. `[S2]` The raw AX numbers reproduce exactly, but all three stated consequences are refuted by measurement. 1. The switch is not an operable component. `.mods-tgl` is a `<span role=switch aria-checked aria-disabled="true" tabind
15. `[S2]` The raw measurement is honest and I reproduced it to the cell — but it is an artifact of the auditor's own instrument, and both the reachability and the consequence fail. 1) REPRODUCED. Injecting `document.documentElemen
16. `[S2]` The structural half reproduces, but the finding's load-bearing measurement is an instrument artifact and its consequence is refuted. REPRODUCES (my numbers, #signals, 1440x900, light): 1 tablist aria-label="time range"; 
17. `[S2]` The finding's arithmetic reproduces perfectly, and its consequence is refuted by two independent measurements. REPRODUCED (I confirm all of these myself): live bundle ships `.skip-link{position:absolute;left:-9999px;top:
18. `[S2]` The pixel measurement reproduces perfectly — I got the same forced-colors values independently — but the finding fails on the exact axis the brief warns about: the number reproduces, the consequence does not. 1. DECORATI
19. `[S2]` The MECHANISM reproduces exactly — I confirmed every number independently. But the CONSEQUENCE is refuted, which is the failure mode this console specialises in. The auditor's stated harm is "scanning degrades to reading
20. `[S2]` The raw geometry reproduces exactly, but the finding fails on three independent grounds: its own headline is factually wrong, the RTL state is not operator-reachable, and the "actively lies" consequence does not survive 
21. `[S2]` The PIXEL claim reproduces exactly — I confirmed it with a stronger, falsifiable instrument than the auditor used. The CONSEQUENCE does not, and the auditor's own pre-emptive rebuttal of it is factually wrong against the
22. `[S3]` The raw numbers reproduce to the element, but the mechanism the finding asserts — "a hard 90-row ring buffer that evicts" — does not exist anywhere in the live console. The finding is an instrument artifact of the audito
23. `[S3]` I re-ran the experiment independently (my own login, 1440x900 viewport, PerformanceObserver type 'layout-shift' buffered, 3.5s settle then a 60,000ms steady-state window per view, 4 views, 240s of steady-state observatio
24. `[S3]` The measurement reproduces exactly, but it is a null result with no operator consequence — the finding's own consequence field says "None observed", and my independent test confirms there is none. I re-measured with my o
25. `[S4]` Every raw number reproduces, and the consequence is not merely unproven — it is inverted. 1) The two surfaces do not read the same population. #actions counts `FROM action_manifest` (core/db/action_manifest_read.go:126);

## S5 — the three lenses that had never been run, plus the medium re-sweep (2026-07-29)

Run as workflow `ux-final-lenses` (27 agents, adversarially verified: 23 candidates -> **19 confirmed, 4 refuted**).
These are the lenses the register listed as never executed: **real screen-reader speech** (Chrome accname +
live-region observation), **multi-tab / concurrent-session**, and **session-boundary flows**. They are now RUN.

| # | lens | sev | finding | status |
|---|---|---|---|---|
| S5-1 | sr-speech | medium | Activating a run in the #workflows "Governed runs" listbox says NOTHING and throws the reading cursor to the top of the document. Focus before Enter is the option `"librenms-dc1-181797 li… | FIXED !734 (focus survival + announce) |
| S5-2 | sr-speech | medium | Expanding a stage on #workflows flips `aria-expanded` false→true on the header but moves focus to `<body>` in the same tick, so the state change is announced on a node the screen reader is n… | FIXED !734 (focus survival + caret aria-hidden) |
| S5-3 | sr-speech | high | On #workflows, focus is destroyed by the live SSE re-render with NO user input at all — the focused option is detached and focus falls to `<body>` within 20 seconds of landing on it. | FIXED !734 (keyed re-render — the unprovoked case) |
| S5-4 | sr-speech | medium | The app-level polite live region announces a FIXTURE verdict, unqualified, on every view including the ones labelled live. `#tgAnnounce` (aria-live=polite, aria-atomic=true) is populated ~20… | FIXED !735 (diegetic simulator deleted) |
| S5-5 | sr-speech | medium | The command-palette opener — the console's only global search/jump affordance — has the accessible name "⌘K" and nothing else. U+2318 is spoken by NVDA/eSpeak and VoiceOver as "place of inte… | FIXED !733 (palOpen aria-label) |
| S5-6 | multi-tab | medium | The guard that says "a REFRESH must not evict a working session on one transient error" is unreachable: two module wrappers reassign the global `liveAdopt` with a zero-arg function that forw… | FIXED !736 (both wrappers forward refresh) |
| S5-7 | multi-tab | medium | #authGate is not a modal: it is a plain `<div class="authgate">` with no role=dialog / aria-modal and nothing inert behind it, so on a re-gated tab the entire dead console stays keyboard-rea… | FIXED !736 (dialog + inert) |
| S5-8 | multi-tab | medium | Signing out does not terminate the already-open SSE stream: /api/v1/events keeps delivering governance posture (mutation_enabled, posture_source, chain seq) to a tab whose session has been d… | FIXED !736 (liveCloseStreams before logout) |
| S5-9 | medium-sweep | medium | REPRODUCES — the command-palette result list is still invisible to assistive tech: 28 result rows with class-based arrow-key selection and no listbox/option semantics. | FIXED !733 (palette combobox) |
| S5-10 | medium-sweep | medium | REPRODUCES — facet chips convey the active filter by CSS class only; not one of the 39 chips carries aria-pressed, aria-selected, or a role, and the filters demonstrably change content. | FIXED !733 (aria-pressed) |
| S5-11 | medium-sweep | low | REPRODUCES — no nav item carries aria-current; the active view is conveyed to sighted users by a class and to AT by nothing. | FIXED !733 (aria-current) |
| S5-12 | medium-sweep | medium | CHANGED — the Methods column is fixed, but the constant moved into the Access column: 7 POST-only routes, including the actuation-mode chokepoint, are labelled "session-ok · GET". | FIXED !736 (Access derived from parsed methods) |
| S5-13 | medium-sweep | medium | REPRODUCES — the seal-form fields and the palette input fall through to the UA default placeholder colour rgb(117,117,117) and fail AA; the base-level rule the original finding described has… | FIXED !733 (::placeholder base rule) |
| S5-14 | medium-sweep | medium | REPRODUCES — two disabled estate actions with no explanation, sitting next to a posture note that contradicts their disablement. | FIXED !736 (controls name the real reason) |
| S5-15 | information-design | medium | On #command — the operator's triage screen, subtitled 'the AI's requests, ranked — does it need me' — all four facets are inert. Clicking NEEDS ME, DEVIATIONS or AUTO visibly activates the b… | FIXED !735 (facets wired to the live spine) |
| S5-16 | information-design | high | #actions labels abandoned and timed-out actions 'EXECUTING'. Status is derived as `a.executed ? "executing" : a.approved ? "executing"`, so every approved-but-never-executed manifest gets th… | FIXED !735 (approval_choice read) |
| S5-17 | information-design | medium | The Ledger renders timestamps in the viewer's browser timezone while Command and Alerts render UTC — both unlabelled, and the Ledger has no date column at all. The same event therefore appea… | FIXED !736 (UTC everywhere, named in the header) |
| S5-18 | information-design | medium | #grounding's falsifiability verdict contradicts the two numbers printed directly beneath it. The headline reads '×0.4 FALSIFIABILITY SIGNAL / at or below chance' with the prose 'The predicti… | FIXED !735 (SignalRatio floor removed) |
| S5-19 | information-design | medium | #regime states the system is in Shadow and cannot actuate, four times, on a screen whose own posture tile says the opposite and while the estate is being actuated. The tile reads 'ACTUATING … | FIXED !735 (posture computed once) |

**Refuted (4)** — recorded so they are not re-raised:

1. `[sr-speech]` On #signals the three controls that open a session announce only an opaque 5-character token — "s-4d90, button", "s-8f31, button", "s-6c55, button". Everything that identifies the session is in a sibl…
2. `[multi-tab]` A tab whose session has died polls unauthenticated endpoints forever with no backoff and no ceiling, because `liveState.on` is never cleared on the 401 path — so the skills and wiki wrappers keep firi…
3. `[multi-tab]` The theme toggle does not propagate between tabs — it writes shared localStorage but no `storage` listener exists, so two tabs of the same operator render in different themes until the loser is reload…
4. `[information-design]` Two live views answer the same question — 'did the prediction match the territory?' — with numbers 24x apart, and neither states its population or window. #actions headlines its strip "PREDICTION (MAP…

## S6 — the 15 UNCOVERED WCAG rows, resolved (2026-07-29)

Run as workflow `ux-uncovered-15-final`, scoped to exactly the rows this register marked UNCOVERED —
no open-ended discovery, which is the whole point of keeping this file. **13 PASS · 1 confirmed · 1 refuted.**
The UNCOVERED list above is now EMPTY.

| SC | level | result |
|---|---|---|
| 1.3.4 Orientation | AA | PASS — no orientation rule anywhere; 390x844 and 844x390 both render all 22 views with no forced horizontal scroll |
| 1.4.5 Images of Text | AA | PASS — zero `<img>` rendered in any view; the only raster is the logotype (exempt); SVG `<text>` is real text |
| 1.4.13 Content on Hover or Focus | AA | PASS — exactly ONE custom hover surface (`.e2-xread`); measured hoverable, persistent, and overlapping nothing (overlapCount 0) |
| 2.1.4 Character Key Shortcuts | A | PASS — 19 bare keys probed, zero effects; only Ctrl+K (a modifier combo, exempt) |
| 2.4.5 Multiple Ways | AA | PASS — nav rail + command palette + hash addressing, three independent routes to every view |
| **2.4.11 Focus Not Obscured (Min)** | **AA** | **CONFIRMED → FIXED !735** — the fixed z-90 toast could fully cover the focused control, AND at opacity 0 stayed a permanent hit-target (`elementFromPoint` over a live button returned the toast's span, forever after the first toast) |
| 2.5.2 Pointer Cancellation | A | PASS — instrumented `addEventListener` before any page script: ZERO down-event handlers from console code |
| 2.5.3 Label in Name | A | PASS |
| 2.5.7 Dragging Movements | AA | PASS — no drag interaction exists |
| 3.2.1 On Focus | A | PASS |
| 3.2.2 On Input | A | PASS |
| 3.2.6 Consistent Help | A | N/A, and consistent even read broadly |
| 3.3.7 Redundant Entry | A | PASS |
| 3.3.8 Accessible Authentication (Min) | AA | PASS on both sign-in and step-up |
| 3.3.4 Error Prevention (Legal/Financial/Data) | AA | **REFUTED** — the code reading was right (liveVote POSTs with no confirm) but the consequence is unreachable as stated; kept here so it is not re-raised |

**The register's scope is now closed.** Every WCAG 2.2 A+AA criterion and every named operator-console
dimension has been either covered by a lens or explicitly resolved above. Any further audit must name a NEW
dimension and add its row here first.

---

## §S7 — the recovered `console-100pct-falsification` run (2026-07-29, FINAL agent pass)

This was the **last agent-driven console audit** by owner instruction. 26 finders + adversarial refuters ran;
10 agents died on API 529 mid-run and were recovered by cache replay, so the findings below include the
lenses that were missing when §S5/§S6 were written: the small-viewport attack, the overlay/nesting lens, and
the focus-restore critic. Full agent transcripts and every refute verdict are preserved verbatim in
`console-falsification-RESULTS.json` (37 agent records) alongside this register.

**The headline finding is a defect in a fix from earlier the same day, and it is the most serious one in the
whole audit.**

### FIXED in this pass

| # | finding | why it mattered |
|---|---|---|
| S7-1 | **`routeFocusKey` restored an actuation control by ORDINAL POSITION.** Approve/Deny carried no id and no `data-*` key, so focus restore fell to "nth same-tag sibling of #view". **Measured live:** operator holds Approve on `tg-liveness-dc1cloudbeaver01-1785313617…`; another row is answered by someone else and vanishes; the next 25s poll puts focus on Approve for an **unrelated action** (`librenms-dc1-1814075… start-guest`). Same label, still attached, nothing visible changed. | Enter after a poll tick actuates a target the operator never read. The queue moves constantly — 17 → 16 → 15 open decisions inside one hour — so this is the ordinary case, not a corner. Fixed two ways: vote buttons now carry `data-appr-key` = (external_ref, action_id, verb), and the ordinal fallback now **refuses** to restore any actuation control it cannot identify by key. Losing focus costs a Tab; landing on a stranger's Approve costs an unintended actuation. |
| S7-2 | **`inert` was not reference-counted across nested overlays.** `overlayClose` released `#appRoot`'s `inert` unconditionally, so closing an overlay opened *on top of* another exposed the entire background while the lower modal was still painted over it. | The background of this console holds the KILL switch and the Approve/Deny strip. Fixed: the stack is the authority — `inert` is released only when no remaining overlay claims it. |
| S7-3 | **`.mods-drawer` was a fifth `role=dialog aria-modal=true` overlay outside the registry** — it told assistive technology the page was unavailable while setting no `inert`, never joining `overlayStack`, and carrying its own competing Escape listener. | Enrolled in the one registry; the ad-hoc Escape/focus handling is deleted rather than duplicated. |

Oracle: `e2e/overlay-nesting-and-actuation-focus.mjs` (12 checks). Four mutation controls PROVEN RED —
unconditional inert release; vote buttons stripped of keys; the unkeyed-actuation guard removed; and a
reference count that never releases (which would leave the console permanently inert). The oracle asserts
**both directions**: a fix that pinned focus forever, or one that merely stopped restoring anything, fails it.

### CONFIRMED and still OPEN

Carried here so they are not lost now that the agent lens is closed. Each was independently reproduced on the
live URL by a refuter. S7-4/5/6 were fixed in !751, S7-7/S7-13 in !752, S7-15 in !753, S7-9 in !754 (the fixture module
switches left the live path entirely when /v1/capabilities became the view), S7-8/S7-10 in !755, and S7-11/S7-12 in !756 (struck through below); the rest remain OPEN.
S7-13's "Floored" half could not be fixed in the client — the server DTO carries no floor fact, so the
facet was REMOVED rather than left unfalsifiable, and projecting the floor from /v1/actions is tracked
as its own task. An absent lens asks the operator to look elsewhere; an unfalsifiable one answers wrongly.

| # | finding | component |
|---|---|---|
| ~~S7-4~~ **FIXED (!751)** | `#annun` clips the POLL_PAUSE band count off the right edge below 1448px — the count of decisions awaiting a human is the one number that must not be invisible | header |
| ~~S7-5~~ **FIXED (!751)** | At ≤820px the CLOSED off-canvas nav rail stays in the tab order while rendered off-screen (`translateX(-230px)`) — the same two-place `inert` defect as S7-2, in the responsive layout | rail |
| ~~S7-6~~ **FIXED (!751)** | The ≤820px rail, once open, cannot be dismissed by Escape or by tapping outside — its `.scrim` is `opacity:0` and has no handler | rail |
| ~~S7-7~~ **FIXED (!752)** | `#workflows` memoises the governed-run list at the first successful `/v1/sessions` read and never invalidates it, so the decision tracer can show runs that no longer exist | workflows |
| ~~S7-8~~ **FIXED (!755)** | `#estatedepth` is 217 tab stops in which the selected node is published only as a CSS class — 0/217 rows carry `aria-current`/`aria-selected` | estate |
| ~~S7-9~~ **FIXED (!754)** | All 31 module switches share one accessible name that never names the module (computed from `title`) | modules |
| ~~S7-10~~ **FIXED (!755)** | `#workflows` declares `role=listbox` with 50 `role=option` children and implements none of the composite-widget keyboard contract | workflows |
| ~~S7-11~~ **FIXED (!756)** | The command palette indexes zero of the 50 live sessions despite an accessible name promising "search views, sessions and hosts"; navigating via it leaves `document.activeElement` on `<body>` | palette |
| ~~S7-12~~ **FIXED (!756)** | `liveAdopt()` performs 22 sequential awaited fetches with no `AbortController` and no timeout, so one merely-SLOW read stalls the whole adoption | live layer |
| ~~S7-13~~ **FIXED (!752)** | `#actions`' "Floored" facet is structurally inert on live data (the live ActionRibbon DTO has no `floored` field), and "Deviations" filters only the fetched 50-row page while the caption on the same strip states the 113-row population | actions |
| ~~S7-15~~ **FIXED (!753)** | The `#estate` edge table claimed "the 80 highest-confidence of 391 edges" while confidence takes exactly TWO values across the whole graph (0.90 ×196, 0.95 ×194) — so the cut lands inside a 194-edge tie and is arbitrary. Worse, the tie left the order to the server's Go-map serialisation: over a 7-minute soak ONE edge was added and 79 of 80 row positions changed, 50 rows vanishing and 50 appearing, describing an estate that had not moved. Fixed with a deterministic tiebreak (confidence, then from/to/rel) and a caption that states the tie and how many equally-confident edges are hidden. | estate |
| S7-14 | `#grounding` pools NEVER-EXECUTED propose-path verdicts into what it labels post-actuation outcome accuracy, and attributes all 935 POLL_PAUSE classifications to the never-auto floor | grounding |

S7-14 is the same measurement problem as the standing `prediction_verdict` 19/19-deviation finding and should
be fixed with it, not separately.

### REFUTED by adversarial verification — do not re-raise

The mutation-breaker posture text clipping below 1080px (the clipping is real; the claim built on it is not),
and four further claims whose measurement reproduced while the stated consequence did not. See the preserved
verdicts for the reasoning in each case.
