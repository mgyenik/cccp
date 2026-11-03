// Package fuzzy scores how well a query matches a text, in the style of
// fzf: a case-insensitive subsequence match, scored higher for consecutive
// characters, matches at word starts and at the start of the text, and
// lower for gaps. A query of several space-separated terms matches only if
// every term does; the score is the sum. Pure, no allocation per character.
//
// A subsequence match is only meaningful in short text: across a paragraph
// almost any query matches, scattered. So MatchFields matches each term
// within a single field, fuzzily in short fields (names, paths) and only as
// a substring in prose (prompts, summaries).
package fuzzy

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Scores.
const (
	scoreMatch       = 16
	bonusConsecutive = 16
	bonusWordStart   = 12
	bonusFirst       = 8
	penaltyGap       = 1
	maxGapPenalty    = 12
)

// Match scores query against one short text. ok is false when some term
// does not match; an empty query matches everything with score 0.
func Match(query, text string) (score int, ok bool) {
	return MatchFields(query, Field{Text: text})
}

// Field is one searchable piece of text. Prose is matched by substring only,
// and scores half as much as the same match in a short field: a term found in
// a title says more than the same term found in a paragraph.
type Field struct {
	Text  string
	Prose bool
}

// MatchFields scores query against fields: every term must match within some
// single field, and contributes its best score over the fields.
func MatchFields(query string, fields ...Field) (score int, ok bool) {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return 0, true
	}
	low := make([]string, len(fields))
	for i, f := range fields {
		low[i] = strings.ToLower(f.Text)
	}
	total := 0
	for _, t := range terms {
		best, found := 0, false
		for i, f := range fields {
			var s int
			var ok bool
			if f.Prose {
				s, ok = matchSubstring(t, low[i])
				s /= 2
			} else {
				s, ok = matchTerm(t, low[i])
			}
			if ok && (!found || s > best) {
				best, found = s, true
			}
		}
		if !found {
			return 0, false
		}
		total += best
	}
	return total, true
}

// matchSubstring scores the best contiguous occurrence of term in text
// (both lower case), with the same scoring as a fuzzy match.
func matchSubstring(term, text string) (int, bool) {
	best, found := 0, false
	for off := 0; ; {
		i := strings.Index(text[off:], term)
		if i < 0 {
			return best, found
		}
		if s, _ := greedy(term, text, off+i); !found || s > best {
			best, found = s, true
		}
		_, w := utf8.DecodeRuneInString(text[off+i:])
		off += i + w
	}
}

// matchTerm finds the best-scoring subsequence occurrence of term in text
// (both lower case). It tries each occurrence of the term's first rune as a
// starting point and keeps the best greedy match from there, which captures
// the common cases (prefix, word start, contiguous) without the cost of a
// full alignment.
func matchTerm(term, text string) (int, bool) {
	first, _ := utf8.DecodeRuneInString(term)
	best, found := 0, false
	for i := 0; i < len(text); {
		r, w := utf8.DecodeRuneInString(text[i:])
		if r == first {
			if s, ok := greedy(term, text, i); ok && (!found || s > best) {
				best, found = s, true
			}
		}
		i += w
	}
	return best, found
}

// minRun is the shortest run of consecutive matched characters that may
// start inside a word, when a match is split into several runs.
const minRun = 3

// greedy matches term against text starting at byte offset start.
//
// A match in one run is a substring and always counts. A match split into
// several runs counts only if every run starts a word (an abbreviation:
// "gs" in "gain staging") or is at least minRun long ("vitrbi" in
// "viterbi": vit|rbi). Otherwise single letters picked out of the middle of
// unrelated words would match: "viterbi" in "an avid kitten, very bitter".
func greedy(term, text string, start int) (int, bool) {
	score := 0
	ti := 0
	prevMatched := false
	gap := 0
	prev := rune(0)
	if start > 0 {
		prev, _ = utf8.DecodeLastRuneInString(text[:start])
	}
	runs, runLen, runAtWord, weak := 0, 0, false, false
	endRun := func() {
		if runLen > 0 && !runAtWord && runLen < minRun {
			weak = true
		}
		runLen = 0
	}
	for i := start; i < len(text) && ti < len(term); {
		r, w := utf8.DecodeRuneInString(text[i:])
		tr, tw := utf8.DecodeRuneInString(term[ti:])
		if r == tr {
			if !prevMatched {
				runs++
				runAtWord = isBoundary(prev, r)
			}
			runLen++
			s := scoreMatch
			if prevMatched {
				s += bonusConsecutive
			}
			if i == 0 {
				s += bonusFirst
			}
			if isBoundary(prev, r) {
				s += bonusWordStart
			}
			if gap > 0 {
				s -= min(gap*penaltyGap, maxGapPenalty)
			}
			score += s
			ti += tw
			prevMatched, gap = true, 0
		} else {
			if prevMatched {
				endRun()
			}
			prevMatched = false
			gap++
		}
		prev = r
		i += w
	}
	endRun()
	if ti < len(term) || (runs > 1 && weak) {
		return 0, false
	}
	return score, true
}

// isBoundary: r starts a word (after a separator or at a lower→upper case
// change; text is lower-cased, so separators decide).
func isBoundary(prev, r rune) bool {
	if prev == 0 {
		return true
	}
	return !unicode.IsLetter(prev) && !unicode.IsDigit(prev) && (unicode.IsLetter(r) || unicode.IsDigit(r))
}
