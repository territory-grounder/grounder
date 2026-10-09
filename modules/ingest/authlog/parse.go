package authlog

// THE LINE PARSER, WRITTEN AGAINST THE ESTATE'S ACTUAL LOGS RATHER THAN AGAINST THE SHAPE OF SSH LOGS.
//
// That distinction cost a design. The plan said "admit root sessions opened over the network", and the
// syslog-ng collection carries 2,943 "session opened" lines in 36 hours. Reading them:
//
//	Aug  5 23:35:01 dc1pve01 CRON[2359236]: pam_unix(cron:session): session opened for user root(uid=0) by root(uid=0)
//
// Almost every one is CRON. Admitting "session opened for user root" would have produced thousands of
// events per day describing the crontab running, drowning the correlator and — through the auto->APPROVE
// clamp — the human approval queue. The pam SERVICE inside the parentheses is what separates a login from
// a scheduled job, and nothing but reading the real lines would have shown that.
//
// So KindRootSession requires pam_unix(sshd:session). A cron or systemd-user session is not admitted, and
// TestCronRootSessionsAreNotAdmitted holds that line.

import (
	"regexp"
	"strings"
	"time"
)

// The patterns below are anchored on the SERVICE and the exact message shape openssh/pam emit, never on a
// loose keyword. "failure" appears in a lot of prose that is not an auth event.
var (
	// `Failed password for [invalid user ]NAME from IP port N ssh2`
	reFailedPassword = regexp.MustCompile(`sshd[^:]*: Failed password for (?:invalid user )?(\S+) from (\S+) port`)
	// `Invalid user NAME from IP` — logged even when no password is attempted.
	reInvalidUser = regexp.MustCompile(`sshd[^:]*: Invalid user (\S+) from (\S+)`)
	// `pam_unix(sshd:auth): authentication failure; ... rhost=IP  user=NAME`
	rePamAuthFailure = regexp.MustCompile(`pam_unix\(sshd:auth\): authentication failure`)
	rePamRhost       = regexp.MustCompile(`rhost=(\S+)`)
	rePamUser        = regexp.MustCompile(`user=(\S+)`)
	// `sshd[..]: Connection closed by authenticating user NAME IP port N [preauth]`
	reAuthAbort = regexp.MustCompile(`sshd[^:]*: Connection closed by (?:authenticating|invalid) user (\S+) (\S+) port`)

	// `sudo:   USER : TTY=... ; COMMAND=...` and the failure variant.
	reSudo = regexp.MustCompile(`sudo(?:\[[0-9]+\])?:\s+(\S+)\s+:`)
	// `su[..]: (to root) USER on pts/0` / `su: pam_unix(su:session): session opened for user root by USER`
	reSu = regexp.MustCompile(`\bsu(?:\[[0-9]+\])?: \(to (\S+)\) (\S+) on`)

	// THE DISCRIMINATOR. pam_unix(<service>:session) — only sshd is a login.
	rePamSession = regexp.MustCompile(`pam_unix\((\w+):session\): session opened for user (\S+)`)

	// `Aug  5 23:35:01 host ...` — the RFC3164 stamp these files carry. No year, so the caller supplies it.
	reStamp = regexp.MustCompile(`^([A-Z][a-z]{2})\s+([0-9]{1,2})\s+([0-9]{2}):([0-9]{2}):([0-9]{2})\s`)
)

var months = map[string]time.Month{
	"Jan": time.January, "Feb": time.February, "Mar": time.March, "Apr": time.April,
	"May": time.May, "Jun": time.June, "Jul": time.July, "Aug": time.August,
	"Sep": time.September, "Oct": time.October, "Nov": time.November, "Dec": time.December,
}

// ParseLine turns one syslog line into an observation, or (Event{}, false) when the line is not a
// security-significant auth event.
//
// `host` is supplied by the caller (the syslog-ng tree is laid out per host) rather than read from the
// line: the hostname field in the line is written by the SENDER, and this module must not let a remote
// sender choose which host an event is attributed to.
//
// `year` is supplied for the same class of reason — RFC3164 stamps carry no year, and guessing "this year"
// silently mis-dates every line read in the first days of January.
func ParseLine(host, line string, year int) (Event, bool) {
	ts := parseStamp(line, year)

	// --- failures, most specific first -------------------------------------------------------------
	if m := reFailedPassword.FindStringSubmatch(line); m != nil {
		return Event{Host: host, Kind: KindFailure, Principal: m[1], SourceIP: cleanIP(m[2]), Count: 1, LastSeen: ts}, true
	}
	if m := reInvalidUser.FindStringSubmatch(line); m != nil {
		return Event{Host: host, Kind: KindFailure, Principal: m[1], SourceIP: cleanIP(m[2]), Count: 1, LastSeen: ts}, true
	}
	if m := reAuthAbort.FindStringSubmatch(line); m != nil {
		return Event{Host: host, Kind: KindFailure, Principal: m[1], SourceIP: cleanIP(m[2]), Count: 1, LastSeen: ts}, true
	}
	if rePamAuthFailure.MatchString(line) {
		e := Event{Host: host, Kind: KindFailure, Count: 1, LastSeen: ts}
		if m := rePamUser.FindStringSubmatch(line); m != nil {
			e.Principal = m[1]
		}
		if m := rePamRhost.FindStringSubmatch(line); m != nil {
			e.SourceIP = cleanIP(m[1])
		}
		return e, true
	}

	// --- escalation --------------------------------------------------------------------------------
	if m := reSu.FindStringSubmatch(line); m != nil {
		return Event{Host: host, Kind: KindEscalation, Principal: m[2], Count: 1, LastSeen: ts}, true
	}
	if m := reSudo.FindStringSubmatch(line); m != nil {
		// `sudo: pam_unix(sudo:session): ...` is the session bookkeeping for an escalation already counted
		// from its command line; admitting both double-counts every sudo.
		if !strings.Contains(line, "pam_unix(sudo:") {
			return Event{Host: host, Kind: KindEscalation, Principal: m[1], Count: 1, LastSeen: ts}, true
		}
	}

	// --- root sessions, sshd ONLY ------------------------------------------------------------------
	if m := rePamSession.FindStringSubmatch(line); m != nil {
		service, user := m[1], strings.SplitN(m[2], "(", 2)[0]
		// THE DISCRIMINATOR. cron/systemd-user/atd open root sessions constantly and legitimately — 2,943
		// of them in 36h on this estate. Only an sshd session is a login.
		if service == "sshd" && user == "root" {
			return Event{Host: host, Kind: KindRootSession, Principal: "root", Count: 1, LastSeen: ts}, true
		}
	}
	return Event{}, false
}

// ParseLines runs ParseLine over a block and folds the result — the whole per-poll pipeline for one host.
func ParseLines(host string, lines []string, year int) []Event {
	var obs []Event
	for _, l := range lines {
		if e, ok := ParseLine(host, l, year); ok {
			obs = append(obs, e)
		}
	}
	return Fold(obs)
}

// cleanIP strips the decoration openssh sometimes adds and returns "" for anything that is not an address.
// A hostname in an rhost field is not an address, and forcing one into the IP grammar would attribute an
// event to a machine that does not exist.
func cleanIP(s string) string {
	s = strings.Trim(strings.TrimSpace(s), "[]")
	if s == "" {
		return ""
	}
	return s // validated by net.ParseIP in ToEnvelope; a non-address becomes empty there
}

// parseStamp reads the RFC3164 timestamp. A line without one yields the zero time, which ToEnvelope treats
// as unknown rather than inventing a value.
func parseStamp(line string, year int) time.Time {
	m := reStamp.FindStringSubmatch(line)
	if m == nil || year <= 0 {
		return time.Time{}
	}
	mon, ok := months[m[1]]
	if !ok {
		return time.Time{}
	}
	day := atoi(m[2])
	if day < 1 || day > 31 {
		return time.Time{}
	}
	return time.Date(year, mon, day, atoi(m[3]), atoi(m[4]), atoi(m[5]), 0, time.UTC)
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}
