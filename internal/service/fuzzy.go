package service

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"kanban/internal/model"
)

// Typo tolerance for a search term, in single-letter edits.
//
// It grows with the term because one wrong letter in four is a different thing
// from one wrong letter in twelve, and a short term gets none at all: at three
// letters almost every word is one edit from every other, so "bug" would find
// "bag", "big" and "but" and the search would stop being a search.
// maxFuzzyTerm is the longest term this will look for a typo in. Fuzzy matching
// is for a word somebody mistyped, and past this a term is not a mistyped word:
// it is a paste, or a URL, or a request built to be expensive. The literal
// match still runs on it, so nothing findable becomes unfindable.
//
// It is a bound on work as much as on meaning. The distance is computed once
// per word of every card the literal match missed, so the cost is the term
// times the board: a 64 kB term over 200 ordinary cards took 1.15 s, and over
// 50 cards at the description cap it took 8.85 s and allocated 22 GB. One
// request, from anybody who can see the board.
const maxFuzzyTerm = 64

func tolerance(term string) int {
	switch n := utf8.RuneCountInString(term); {
	case n < 4, n > maxFuzzyTerm:
		return 0
	case n < 8:
		return 1
	default:
		return 2
	}
}

// closeToAny reports whether term is a typo away from any of words.
func closeToAny(term string, words []string) bool {
	max := tolerance(term)
	if max == 0 {
		return false
	}
	for _, w := range words {
		if withinDistance(term, w, max) {
			return true
		}
	}
	return false
}

// cardWords is everything a bare word is matched against, split into words: the
// title, the description and the names of the labels the card carries. All three
// are lowercase already, the first two because the caller lowercased them and
// the label names because Match takes them that way, which is what the literal
// label match relies on too.
//
// Words and not the whole text, because the distance between a mistyped word and
// a paragraph is roughly the length of the paragraph. Compared as a whole, a
// typo would only ever find a card whose description is as short as the term.
func cardWords(title, description string, c model.Card, labelNames map[model.ID]string) []string {
	words := append(splitWords(title), splitWords(description)...)
	for _, id := range c.Labels {
		words = append(words, splitWords(labelNames[id])...)
	}
	return words
}

// splitWords cuts on everything that is not a letter or a digit, so that
// punctuation and the markdown around a word are not part of it.
func splitWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// withinDistance reports whether a and b are at most max edits apart, where an
// edit is a letter inserted, removed or replaced, or two neighbouring letters
// swapped.
//
// The swap is why this is not plain Levenshtein, which counts one as two edits.
// Typing "logni" for "login" is the typo people actually make, and at a
// tolerance of one it is the one that would not be forgiven.
//
// Three rows rather than the whole matrix, and runes rather than bytes so that a
// mistyped umlaut counts as one edit and not as two.
func withinDistance(a, b string, max int) bool {
	// A difference in length is a lower bound on the distance: the shorter
	// string needs at least that many letters inserted to reach the longer one.
	// Counted before the strings are converted, not after: the conversion
	// allocates four bytes per byte of the term and was being done once per
	// word of every card, only to be thrown away on this line.
	if diff := utf8.RuneCountInString(a) - utf8.RuneCountInString(b); diff > max || -diff > max {
		return false
	}
	ar, br := []rune(a), []rune(b)
	prev2 := make([]int, len(br)+1)
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && ar[i-1] == br[j-2] && ar[i-2] == br[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
			best = min(best, cur[j])
		}
		// Every path to the end of the matrix runs through this row, and no step
		// makes the distance smaller, so a row that is already past max cannot
		// come back under it.
		if best > max {
			return false
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(br)] <= max
}
