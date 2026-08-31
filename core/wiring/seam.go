// Package wiring makes a DARK SEAM impossible to ship silently.
//
// THE DISEASE THIS TREATS. This repo's documented pathology is code that exists and is never called —
// green oracles certifying an unreachable path. The live specimen: zero notifiers were configured, so
// `deps.Notify` was nil and every governance notice, including a judge-death page, degraded to a
// `log.Printf` on worker stdout that reaches no operator. Nothing anywhere reported it. A real page
// fired on 2026-08-01T00:00:00Z and was verified afterwards, from the container logs, by hand.
//
// WHY A REGISTRY OF DECLARATIONS WOULD NOT HAVE CAUGHT IT. Any check that asks "is a notifier module
// ENABLED?" answers yes the moment one is configured, while `deps.Notify` can still be nil — the count
// and the binding are different facts. Worse, the notifier is about to become Matrix + SMS + email, and
// `main.go`'s `notifierCount > 1` branch leaves the sink nil too: a registry-count predicate reports
// LIVE on precisely the configuration that delivers nothing.
//
// SO LIVENESS IS DERIVED FROM THE BOUND VALUE, NEVER ASSERTED BY THE CALLER. `Bind` reflects on what it
// was actually handed; there is deliberately no `live bool` parameter, because a parameter is a place to
// put a lie. `Absent` is the only other way to satisfy a seam, it must be called at the same site that
// would have bound, and it carries a reason, a consequence, an owner, a ticket and an expiry. A Critical
// seam refuses `Absent` outright unless an operator names it in the environment — so accepting a dark
// page path is an auditable act with a metric series, not a source edit that reads like normal wiring.
//
// Provenance: [F] the predecessor's registry-check.py (387 components, 46 declared known-dark, exit 1
// iff a critical one is dark) — re-expressed structurally, because a hand-maintained manifest is the
// same class of artifact that produced "26/26 findings closed" over a subsystem it never audited.
package wiring

// Seam is one place in the composition root where a capability is either bound to something real or is
// not. The set is CLOSED (see All) so that a seam nobody remembered to record still reports.
type Seam string

const (
	// SeamGovNotify is `Deps.Notify` — the sink every governance notice and approval page travels
	// through. Nil here is silent, total, and was live in production when this package was written.
	SeamGovNotify Seam = "gov.notify"

	// SeamEscalationPage is the pager the escalation requeue lane fires through. Its darkness is subtler
	// and worse than SeamGovNotify's: notifierPager is a perfectly non-nil STRUCT whose Page() returns
	// nil — success — when its inner notify field is nil, so the controller records the page as
	// delivered. And FireDue marks the row fired BEFORE paging (deliberately, to prevent page storms), so
	// the row is consumed, the page vanishes, and nothing ever retries. Every layer reports success.
	SeamEscalationPage Seam = "escalation.page"

	// SeamLessonsFeed is the OPERATOR-EXPORT lane into the precedent corpus: a curated resolved-incident
	// file (TG_LESSONS_SOURCE_FILE) folded in on a timer.
	//
	// IT IS NOT THE ONLY GROWTH PATH, and an earlier draft of this comment wrongly said it was. The live
	// close-out feeder (deps.LearnResolved, main.go — "novelty writeback: live close-out feeder armed")
	// also merges confirmed-clean resolutions into the same corpus, and it IS wired in production. So a
	// dark feed does not mean a frozen corpus; it means the corpus grows ONLY from TG's own confirmed
	// heals, never from curated operator knowledge — no imported history, no hand-written precedent, and
	// nothing at all while the estate is quiet (the deployed corpus file had not been written in 24h).
	//
	// Found dark in production 2026-08-01, with zero log lines saying so, because the outer `if` had no
	// else.
	SeamLessonsFeed Seam = "lessons.feed"

	// SeamWikiCompile is the lane that turns what TG has RECORDED into what an operator can READ: the
	// per-host article compiler (core/wikicompile) writing an envelope the grounder serves at /v1/wiki.
	//
	// Dark, this seam costs an operator the only surface that answers "what has TG actually seen on THIS
	// machine?" from the whole spine. The console's host pane falls back to a client-side filter over a
	// 200-row estate-wide session window — which for 78 hosts cannot give most of them a single incident,
	// and which drops entirely any host that has sessions but no estate-graph node.
	SeamWikiCompile Seam = "wiki.compile"

	// SeamWorldDiscovery is the re-discovery and drift pass that DRAFTS the world model
	// (temporal/worlddiscovery, spec/027 REQ-2705). Without it nothing ever proposes an entity, so
	// manifest_entry stays empty forever and the earned-catalog ladder — propose, candidate, ratify,
	// manifest — has no top rung.
	//
	// Found dead in production 2026-08-01 with manifest_entry at 0 rows: the package was built,
	// documented, unit-tested and GREEN IN CI while being referenced by nothing outside itself. The
	// console rendered "Discovery has not drafted anything yet", which reads as "it looked and found
	// nothing" when the truth was that it had never run. No seam covered the lane, so the boot report
	// had nothing to say about it either.
	SeamWorldDiscovery Seam = "world.discovery"

	// SeamSuppression is TIER-1 ADMISSION: the freeze/fold/dedup/pattern/schedule chain that decides which
	// alerts are worth spending a session on at all.
	//
	// Its darkness was UNDECLARED until 2026-08-01, which is worse than the worlddiscovery case rather
	// than better. That lane was code nobody called; this is a guard nobody declared — the whole chain is
	// assembled inside `if len(windows) > 0 || …` with no else, every TG_SUPPRESSION_* key ships empty,
	// and the boot dark-report was therefore silent about a safety-adjacent plane being entirely off.
	//
	// Dark here does NOT mean unsafe: with no chain every incident is investigated, which is the
	// fail-open direction and the correct one. It means TG spends a session on alerts an operator has
	// declared expected — maintenance windows, known flaps, scheduled reboots — and nothing says so.
	SeamSuppression Seam = "suppression.tier1"

	// SeamTrackerEntry is the incident's OWN TICKET — the entry tracker the investigation reads, the
	// reconcile close-out transitions, the learned-reboot lane consults, and the dedup stage asks whether
	// a parent incident is still open.
	//
	// It was bound only when EXACTLY ONE tracker was enabled, with no else arm and nothing recording the
	// gap. Two configured trackers — ServiceNow for ITSM and YouTrack for engineering, the ordinary shape
	// at an established site — took all four capabilities dark at once and said nothing. The fourth WAS a
	// WRONG ANSWER until TG-354: with the OpenIssue check nil, core/suppression/dedup.go suppressed a
	// re-fire whose parent ticket had already resolved under the reason "duplicate of an open incident
	// within window", asserting an openness nothing checked. The dedup stage now fails toward surfacing —
	// a suppression must be BACKED by a confirmed-open parent — so that unconfirmable case escalates.
	SeamTrackerEntry Seam = "tracker.entry"

	// SeamTrackerImport is the COMPOUNDING lane for tracker history: it distils the estate's own incident
	// tracker (ServiceNow / YouTrack / Jira) into ranked precedent rows (ProvenanceTrackerImport, TG-244) in
	// the maintained corpus, so what the tracker knows reaches the retriever's ranking and the next session.
	//
	// It is the WRITE half of a pair whose read half already shipped. get-tracker-history (MR !845) is a
	// read-only agent tool that FETCHES prior incidents on demand but persists nothing, so recall does not
	// compound: every session re-reads the same tickets, and the human resolutions the site's engineers
	// already wrote never reach the ranking or the wiki. This seam closes that loop. Dark when no
	// history-capable tracker is configured, or when there is no durable corpus (TG_KNOWLEDGE_FILE) to
	// compound into — the same shape as SeamLessonsFeed, and safe the same way: nothing is un-reached, the
	// corpus simply does not grow from the estate's own ticket archive.
	SeamTrackerImport Seam = "tracker.import"

	// SeamDiscoveryService is the SERVICE-OBSERVING half of estate discovery: the systemd-unit and docker-
	// container probes that are the only producers of estate.TypeService.
	//
	// It exists because world.discovery being LIVE was not the same as world discovery working. Both probe
	// packages were linked into NO BINARY, and core/worldmodel routes exclusively TypeService to KindUnit
	// and KindContainer — so two of the three adoption kinds could never receive a drafted entry, while the
	// world.discovery seam reported live and the boot log announced "armed every 30m over N source(s)".
	// A lane can be live in one kind of three, and one seam per LANE cannot say so. This one covers the
	// half, because the half is what was dark.
	SeamDiscoveryService Seam = "discovery.service"

	// SeamVoteInbound is the MAILBOX for the approval ballot TG posts.
	//
	// Six notifier modules implement ResolveVote and NOTHING called any of them: TG posted MSC3381 polls a
	// human could click, and the click went nowhere. Votes only ever arrived through the console. This seam
	// exists so "the ballot is unanswerable" is a reported state rather than something an operator
	// discovers by clicking and watching nothing happen.
	//
	// It is also what the approver-set field must render its ARMED status from. An approver list that is
	// configured but consumed by nothing is a security control that looks enforced and enforces nothing,
	// and the field's own non-emptiness can never reveal that.
	SeamVoteInbound Seam = "vote.inbound"

	// SeamHostDiag is the agent's READ OF THE ALERTING HOST ITSELF — check-host-services / -disk /
	// -memory / -load over host-key-verified SSH.
	//
	// It exists because this lane failed 100% of the time, for weeks, and NOTHING said so (TG-271). The
	// tools were registered, the boot log announced "registered 4 read-only host-diagnostics tools across
	// 1 access rule(s)", the module was configured, and every read came back "(<host> was unreachable or
	// the read errored)" because TG_HOSTDIAG_KNOWN_HOSTS covered 16 of the 38 estate hosts TG alerts on.
	// Host-key verification fails CLOSED, so a host missing from that file is a host the agent can never
	// diagnose — invisible at configure time, surfacing only as a triage session that stands down without
	// naming the failing unit, mid-incident.
	//
	// The manifest cannot catch this: the seam is BOUND and RUNNING. It is running and producing nothing,
	// which is the half the yield register exists for.
	SeamHostDiag Seam = "hostdiag.read"

	// SeamSyslogRead is the agent's READ OF THE DEVICE'S OWN SYSLOG — get-host-logs / search-host-logs
	// against the per-site syslog-ng servers.
	//
	// It joins the set with the per-session search cap (TG-297), and the cap is the reason it must be
	// here. A budget refusal returns a perfectly good string, exactly as hostdiag's unreachable-host
	// sentinel did: any check that counts INVOCATIONS sees a busy, healthy lane while the investigation
	// is being told "no" on every search. A cap that quietly eats an investigation's reads is the same
	// class of invisible as a lane that fails every read, and it must be as visible.
	//
	// The same instrumentation covers the older failure this lane can have in production and never
	// reported: TG_SYSLOGNG_KNOWN_HOSTS unset or short a server's host key, which makes every read refuse
	// fail-closed with a cheerful boot log in front of it.
	SeamSyslogRead Seam = "syslogng.read"
)

// Criticality decides what a DECLARED-dark seam costs. Normal: a validated Because is enough. Critical:
// a Because is not enough — an operator must additionally name the seam in TG_WIRING_ACCEPT_DARK, which
// puts the acceptance in the environment, in the compose file, in the ledger and on a gauge.
type Criticality int

const (
	Normal Criticality = iota
	Critical
)

// Unit declares what a seam is supposed to turn into what, in the OPERATOR's vocabulary — the
// denominator and the numerator of its runtime yield.
//
// Both halves are required because the pair is the whole point: a seam reporting only what it PRODUCED
// cannot distinguish "nothing arrived" from "everything arrived and was dropped", and those are an idle
// estate and a broken lane. Offered without Produced is equally useless. See yield.go.
type Unit struct {
	// Offered names what arrives at the seam (e.g. "governance notices").
	Offered string
	// Produced names what it emits (e.g. "delivered notices").
	Produced string
}

// SeamSpec is the declaration of a seam's existence and what its darkness costs. Consequence is prose
// and is carried VERBATIM into the ledger row and the boot report: a finding that says only
// "gov.notify: dark" tells an operator nothing they can act on.
type SeamSpec struct {
	// Cause is why this seam would be DARK — the nil field, the unset variable, the absent probe. It is
	// printed ONLY in the dark report (core/wiring/report.go), never in the starved or unobserved one.
	//
	// It exists because one string could not answer two questions. Every Consequence used to open with its
	// cause, and yield.go's renderer worked around that by wrapping the text in "[the cost is the same as
	// if it had never been wired — …]" — a true frame around a false sentence. Audited 2026-08-06 against
	// the running worker: FIVE of six such texts named a state the deployment was not in
	// ("no entry tracker is bound" while the boot log read "tracker: entry ticket read/transitioned via
	// youtrack"). Splitting the two fields makes the starved report correct by construction and gives the
	// cause back to the report where it is true.
	//
	// Optional: a seam with no distinctive cause simply omits it.
	Cause       string
	ID          Seam
	Criticality Criticality
	Consequence string
	// Unit is the seam's runtime yield vocabulary. A seam that declares one and that nothing calls
	// YieldRegister.Observe for reports UNOBSERVED — the register's vacuity floor.
	Unit Unit
}

// All is the CLOSED SET of seams. Report ranges over this — never over what was recorded — so a seam
// that no code path touched is reported as dark rather than being invisible.
//
// It is kept SMALL on purpose. The predecessor's equivalent tracks 387 components and 46 declared-dark
// ones, and nothing ever made adding the 47th visible; a set seeded by a sweep is a set nobody reads.
// This one grows one seam per merge request, each arriving with the oracle that proves it can fail.
func All() []SeamSpec {
	return []SeamSpec{

		{
			ID:          SeamSuppression,
			Criticality: Normal, // fail-OPEN: no chain means every incident is investigated, never fewer
			// STATE-NEUTRAL, the second verified instance of the tracker.entry correction. This opened "no
			// tier-1 suppression chain is configured:" — which is the wording of the OTHER boot branch. The
			// running worker prints the first one:
			//
			//   suppression: tier-1 gate active — 0 freeze, 0 fold(s), 0 schedule(s), 0 pattern(s),
			//                0 rule(s), dedup 10m0s
			//
			// (measured 2026-08-06, alongside `suppression.tier1: starved — 171 alerts admitted, 0
			// suppressed`). The gate is ACTIVE with an empty rule set, which is a different thing from
			// unconfigured and has a different remedy: nothing is missing from the deployment, the operator
			// has declared no windows or patterns yet. The old text sent a reader looking for absent
			// configuration.
			//
			// The cost below is identical whether the chain is absent or empty, which is what makes it safe
			// to print in both the dark and the starved report.
			Cause: "no tier-1 suppression chain is configured",
			Consequence: "nothing is suppressed before triage: TG spends a full triage session on " +
				"alerts an operator has already declared expected — declared maintenance windows, known " +
				"flap patterns, scheduled reboots and duplicate re-fires all reach the model",
			Unit: Unit{Offered: "alerts admitted to the tier-1 chain", Produced: "alerts actually suppressed"},
			// PRODUCED is suppressions, not decisions. Every admitted alert yields a decision, so a
			// decision-counting unit could never read starved. What is worth an alarm is a chain that is
			// CONFIGURED — freeze windows, folds, dedup, patterns — and matches nothing: the
			// vacuous-predicate class, where a rule set quietly stops matching and every alert sails
			// through a gate an operator believes is filtering. A chain SHOULD pass most alerts, so the
			// bar is literally zero suppressions over a non-zero intake.
		},
		{
			ID:          SeamWorldDiscovery,
			Criticality: Normal, // nothing is un-reached; TG simply never proposes an entity
			Consequence: "the world-model discovery pass never runs: manifest_entry stays empty, no entity " +
				"is ever drafted for an operator to approve, and the console's manifest surface reads " +
				"\"Discovery has not drafted anything yet\" over a lane that has never run at all",
			Unit: Unit{Offered: "entities observed by the estate sources", Produced: "manifest drafts written"},
		},
		{
			ID:          SeamWikiCompile,
			Criticality: Normal, // nobody is un-reached; the wiki simply stops describing the estate
			Consequence: "TG_WIKI_ARTICLES_FILE is unset: no per-host article is ever compiled, so the " +
				"console's host surface stays a client-side filter over a 200-row estate-wide session " +
				"window (most of 78 hosts get no incident at all) and any host absent from the estate " +
				"graph gets no page whatsoever",
			Unit: Unit{Offered: "hosts and families in the spine", Produced: "articles compiled"},
		},
		{
			ID:          SeamLessonsFeed,
			Criticality: Normal, // not a paging path — nobody is un-reached; TG simply stops learning
			Cause:       "TG_LESSONS_SOURCE_FILE is unset",
			Consequence: "TG_LESSONS_SOURCE_FILE is unset: the corpus can grow ONLY from TG's own " +
				"confirmed-clean heals (the live writeback lane) — never from curated or imported " +
				"operator knowledge, and not at all while the estate is quiet",
			Unit: Unit{Offered: "candidate resolved incidents", Produced: "precedents merged into the corpus"},
		},
		{
			ID: SeamHostDiag,
			// Normal: a dark hostdiag lane un-grounds triage, it does not actuate anything. The agent
			// stands down instead of proposing on a host it could not read — the safe direction, and
			// precisely why it stayed invisible for weeks.
			Criticality: Normal,
			Consequence: "the agent cannot read the alerting host: every resource and service alert is " +
				"triaged without check-host-services / -disk / -memory / -load, so a session that could " +
				"have named the failing unit stands down having never reached the ground truth — and the " +
				"operator sees a plausible, well-reasoned walk with a hole in the middle of it",
			Unit: Unit{
				Offered:  "host-diagnostic reads attempted",
				Produced: "reads that returned host output",
			},
			// PRODUCED is reads that came back with DATA, not reads that returned. Every attempt returns
			// something — the failure path returns the "(host was unreachable or the read errored)"
			// sentinel — so a unit counting returns could never read starved, which is the exact shape of
			// the defect: a lane that answers on every call and answers nothing.
		},
		{
			ID: SeamSyslogRead,
			// Normal: a dark syslog lane un-grounds triage on the hosts it covers, it actuates nothing.
			//
			// ★ THIS CONSEQUENCE USED TO BE BACKWARDS, AND IT WAS NEVER MEASURED. It read "every firewall,
			// switch and router incident is triaged without the device's own syslog". Probed 2026-08-06
			// against dc1syslogng01 through the production read guard — all 126 monitored dc1 hosts,
			// no sampling, probed count asserted against the enumeration before believing the total:
			//
			//	ships logs today  15 / 126
			//	  ankh dc1actualbudget01 dc1ap01 dc1ap02 dc1ap03 dc1atlantis01
			//	  dc1fw01 dc1haproxy01 dc1haproxy02 dc1k8s-node01 dc1k8s-node04
			//	  dc1pve01 dc1pve02 dc1rtr01 dc1sw01
			//
			// The firewall, the router and the switch are all in the COVERED set, as are all three APs and
			// both hypervisors. Network gear is the best-covered class on the estate; the 111 uncovered are
			// overwhelmingly application guests.
			//
			// A register that names the wrong consequence sends the reader to the wrong repair — here, to
			// the devices that already work. That is the same defect this register exists to catch, applied
			// to itself, and the third instance this week after SidecarDown's summary and the
			// baseline-freshness dead-man. The text below states only what was measured (TG-363).
			Criticality: Normal,
			Consequence: "the agent has no device-log window on the hosts that ship logs: measured 2026-08-06, " +
				"15 of 126 monitored dc1 hosts have a syslog-ng file — the network gear (fw01, rtr01, sw01, " +
				"the APs, both hypervisors) is COVERED and the 111 uncovered are overwhelmingly application " +
				"guests, so a starved lane costs the grounded device-log quote on exactly the incidents where " +
				"it was available — and a per-session search budget that is being spent every time looks " +
				"exactly the same from outside",
			Unit: Unit{
				Offered:  "syslog reads attempted",
				Produced: "reads that reached the log",
			},
			// PRODUCED is reads that REACHED a log, not reads that returned. A zero-match grep counts (it
			// ran, and "not in the log" is a grounded observation); a refusal does not — an unroutable
			// host, an unverified host key, and a spent per-session cap all return a well-formed string
			// while the agent learns nothing, which is precisely why counting invocations cannot work.
		},
		{
			ID: SeamVoteInbound,
			// Normal: nothing is un-reached — the console vote path is unaffected and remains the
			// authoritative one. What is lost is that a poll posted to chat cannot be answered there.
			Criticality: Normal,
			// STATE-NEUTRAL (TG-354), the fifth and last of the six cause-asserting consequences to be
			// corrected. This opened "no inbound vote reader is running:", which is false here — the boot
			// log says "votes: inbound matrix reader armed (every 15s) — an approval poll clicked in chat
			// now reaches the waiting decision". The seam is bound and reports UNOBSERVED, meaning nothing
			// measures its yield; the old text sent a reader looking for an unarmed reader instead.
			//
			// discovery.service keeps its cause assertion deliberately: it is DECLARED-DARK on this
			// deployment ("discovery: NO service-observing probe configured"), and for a genuinely dark seam
			// naming the missing thing is the most useful sentence available.
			Cause: "no inbound vote reader is running",
			Consequence: "a poll clicked in chat reaches no decision: an approval poll TG posts can be " +
				"clicked and the click reaches nothing, so the approver set governs no inbound path and " +
				"every decision must be voted through the console",
			Unit: Unit{Offered: "inbound room events", Produced: "votes delivered to a waiting decision"},
		},
		{
			ID: SeamDiscoveryService,
			// Normal: nothing is un-reached and no wrong answer is produced. An operator simply keeps
			// hand-typing units into TG_ACTUATION_ALLOWED_UNITS instead of adopting an observed one —
			// which is toil, not an incident. Critical would force every deployment that does not run the
			// probes to file a waiver to boot clean.
			Criticality: Normal,
			Cause:       "no service-observing discovery probe is wired",
			Consequence: "no service-observing discovery probe is wired: estate.TypeService has no " +
				"producer, so the world model can never draft a KindUnit or KindContainer entry and " +
				"TG_ACTUATION_ALLOWED_UNITS stays the only way a unit becomes an actuation target — " +
				"while the world.discovery seam still reports LIVE for the host/VM kinds that do work",
			Unit: Unit{Offered: "hosts probed", Produced: "service edges observed"},
		},
		{
			ID: SeamTrackerEntry,
			// Normal, not Critical: with NO tracker configured nothing is un-reached — TG simply never
			// reads or writes a ticket, which is a legitimate deployment. Critical would force every
			// trackerless deployment to name this seam in TG_WIRING_ACCEPT_DARK to boot clean, which
			// turns a supported configuration into a waiver.
			Criticality: Normal,
			// THE CAUSE IS NAMED CORRECTLY NOW (TG-354). This Cause used to read "no entry tracker is bound",
			// which is FALSE in the deployment that runs: the boot log says "tracker: entry ticket
			// read/transitioned via youtrack", the module TEST proves it, and the yield register reported
			// `tracker.entry: starved — 299 entry-ticket lookups offered, 0 tickets read` — a tracker IS
			// bound and the seam still yields nothing. The old cause would have sent an operator to check a
			// setting that was already correct. The real, deeper cause holds whether or not a tracker is
			// bound: TG looks the ticket up by its OWN incident external_ref, and the estate's tracker issue
			// ids are a DIFFERENT namespace with nothing mapping between them, so no entry-ticket lookup can
			// ever resolve. Cause is printed ONLY in the dark report; the Consequence states the COST and is
			// state-neutral (printed in both the dark and the starved report), so one string never has to
			// name two different causes.
			Cause: "TG looks the incident up by its own external_ref, which the estate's tracker ids never " +
				"contain — no entry-ticket lookup can resolve, whether or not a tracker is bound",
			Consequence: "the incident's own ticket is not read: the investigation cannot see it or its human " +
				"discussion, the terminal reconcile close-out transitions nothing, the learned " +
				"scheduled-reboot lane runs without a tracker, and the dedup stage cannot confirm whether a " +
				"parent incident is still open — so a re-fire after that incident RESOLVED, or (when no tracker " +
				"can confirm openness) once the prior anchor is no longer fresh, is surfaced for triage rather " +
				"than silenced as a duplicate",
			Unit: Unit{Offered: "entry-ticket lookups", Produced: "tickets read or transitioned"},
		},
		{
			ID: SeamTrackerImport,
			// Normal: with no history-capable tracker (or no durable corpus) nothing is un-reached — TG
			// simply never compounds the estate's ticket archive into ranked precedent, which is the
			// fail-safe direction. Critical would force every deployment without a searchable tracker to file
			// a waiver to boot clean, turning a supported configuration into one.
			Criticality: Normal,
			Cause:       "no history-capable tracker is configured, or TG_KNOWLEDGE_FILE is unset",
			// STATE-NEUTRAL: this seam is BOUND-and-starved whenever a tracker is configured but the import
			// merged nothing new (every candidate a duplicate, dropped by screening, or downhill), so the
			// text states the COST rather than naming the absent thing — the cause lives above and prints
			// only in the dark report.
			Consequence: "the estate's own incident tracker is never distilled into ranked precedent: the " +
				"human resolutions its engineers already wrote for these exact faults stay fetch-only via " +
				"get-tracker-history — re-read every session, never reaching the retriever's ranking, the " +
				"wiki, or the next session, so recall never compounds",
			Unit: Unit{Offered: "historical tracker incidents distilled", Produced: "precedents merged into the corpus"},
			// PRODUCED is precedents MERGED, not incidents fetched. An import that pulls hundreds of tickets
			// and merges zero — all duplicates of rows already held, or all dropped by the screen — is the
			// starvation case, and it reads identically to a healthy idle lane from any counter that tallies
			// fetches. The bar is a non-zero fetch that yields no new precedent.
		},
		{
			ID:          SeamEscalationPage,
			Criticality: Critical,
			// STATE-NEUTRAL (TG-354). This opened "notifierPager.notify is nil:", which is false on this
			// deployment: the boot log says "escalation requeue lane: durable store wired (per-incident cap
			// 3, re-check delay 15m0s) — fires via the FireDue cron, PAGES VIA THE NOTIFIER" and "escalation
			// FireDue: cron armed (*/5 * * * *)". The lane is bound; the register reports this seam
			// UNOBSERVED, meaning nothing measures its yield — a different problem with a different remedy.
			Cause: "notifierPager.notify is nil",
			Consequence: "a due escalation reaches no operator: FireDue marks the queue row fired, Page() " +
				"returns success, and the escalation is permanently lost with no retry and no error anywhere",
			Unit: Unit{Offered: "escalations due", Produced: "pages delivered"},
		},
		{
			ID:          SeamGovNotify,
			Criticality: Critical,
			// STATE-NEUTRAL (TG-354). This opened "deps.Notify is nil:", which is false on this deployment:
			// the boot log says "notifier: governance notices/polls delivered via matrix" and notifier/matrix
			// is among the 10 modules whose TEST can prove them. The seam is bound and UNOBSERVED — nothing
			// reports its yield — which is not the same as unwired and is not fixed by looking for a missing
			// notifier.
			Cause: "deps.Notify is nil",
			Consequence: "governance notices reach no operator: every notice and the judge-death page " +
				"degrade to log.Printf on worker stdout and reach NO operator surface",
			Unit: Unit{Offered: "governance notices and approval polls", Produced: "notices delivered to an operator"},
		},
	}
}

// spec returns the declaration for a seam, and reports whether it is in the closed set at all.
func spec(s Seam) (SeamSpec, bool) {
	for _, sp := range All() {
		if sp.ID == s {
			return sp, true
		}
	}
	return SeamSpec{}, false
}
