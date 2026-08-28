package analyst

import (
	"regexp"
	"strconv"
	"strings"
)

// forbidden is the set of constructions that violate the governance charter's
// rules G-1 (no certainty), G-9 (no advice) and G-10 (no positioning claims).
//
// The patterns are conservative on purpose. A false positive costs a fluent
// narrative and falls back to the deterministic template, which is a small
// price. A false negative ships a sentence claiming an instrument *will* rise,
// which is the thing this platform must never do.
var forbidden = []struct {
	name    string
	pattern *regexp.Regexp
	why     string
}{
	{
		"certainty_directional",
		regexp.MustCompile(`(?i)\b(will|shall|is going to|is set to|is poised to)\s+(rise|fall|climb|drop|increase|decrease|rally|decline|surge|plunge|go\s+(up|down)|move\s+(higher|lower)|break\s+out|reverse)\b`),
		"states a future move as fact rather than as a probability",
	},
	{
		"certainty_adverbs",
		regexp.MustCompile(`(?i)\b(guaranteed|risk-free|riskless|certain to|definitely|undoubtedly|without a doubt|sure thing|can't lose|cannot lose)\b`),
		"asserts certainty",
	},
	{
		"advice",
		regexp.MustCompile(`(?i)\b(you should (buy|sell|short|hold|enter|exit|add|trim)|we recommend|i recommend|recommended (buy|sell)|advise (buying|selling)|time to (buy|sell)|load up|back up the truck)\b`),
		"gives investment advice",
	},
	{
		"price_target_claim",
		regexp.MustCompile(`(?i)\b(price target of|will reach|will hit|headed to|going to \$)`),
		"asserts a price target as an expectation",
	},
	{
		"institutional_positioning",
		regexp.MustCompile(`(?i)\b(institutions are (buying|selling|accumulating|distributing)|smart money is|whales are|institutional (accumulation|distribution)|dark pool (buying|selling))\b`),
		"claims knowledge of who holds a position and why",
	},
	{
		"insider_or_manipulation",
		regexp.MustCompile(`(?i)\b(insider information|front-run|front run|pump and dump|guaranteed return)\b`),
		"references market abuse",
	},
}

// hedged detects a nearby hedge that legitimises otherwise-forbidden phrasing,
// e.g. "if the breakout holds, price will likely move higher" inside an
// explicitly conditional sentence.
var hedged = regexp.MustCompile(`(?i)\b(if|should|were|assuming|conditional on|in the event|scenario)\b`)

// Guard checks a generated narrative for forbidden constructions. It returns
// the reason and true when the text must be rejected.
func Guard(text string) (string, bool) {
	lower := strings.ToLower(text)
	for _, f := range forbidden {
		loc := f.pattern.FindStringIndex(lower)
		if loc == nil {
			continue
		}
		// Only the certainty-directional pattern is excused by hedging, and
		// only when the hedge is in the same sentence.
		if f.name == "certainty_directional" && hedgedInSentence(lower, loc[0]) {
			continue
		}
		return f.name + ": " + f.why + " (matched " + strconv.Quote(lower[loc[0]:loc[1]]) + ")", true
	}
	return "", false
}

// hedgedInSentence reports whether the sentence containing position pos carries
// a conditional marker.
func hedgedInSentence(text string, pos int) bool {
	start := strings.LastIndexAny(text[:pos], ".!?\n")
	if start < 0 {
		start = 0
	}
	end := strings.IndexAny(text[pos:], ".!?\n")
	if end < 0 {
		end = len(text) - pos
	}
	return hedged.MatchString(text[start : pos+end])
}
