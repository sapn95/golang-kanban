package service

import (
	"testing"
	"time"

	"kanban/internal/model"
)

func TestATypoStillFindsTheCard(t *testing.T) {
	today := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	names := map[model.ID]string{"l1": "regression"}
	card := model.Card{
		Title:       "Fix the login page",
		Description: "The form does not receive the token",
		Labels:      []model.ID{"l1"},
	}

	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"the word as it is spelled still matches", "login", true},
		{"a letter left out", "lgin", true},
		{"a letter too many", "loggin", true},
		{"a letter wrong", "lonin", true},
		// The typo people actually make. Counted as two edits by plain
		// Levenshtein, which at this length would not forgive it.
		{"two letters swapped", "logni", true},
		{"a swap in a longer word", "regressoin", true},
		{"a word in the description", "tokne", true},
		{"a label name", "regresion", true},
		{"two edits in a short word is too far", "lomen", false},
		{"two edits in a long word is not", "regresoin", true},
		// Under four letters everything is one edit from everything, so a short
		// term is matched as it was typed and no other way.
		{"a short word is exact", "fex", false},
		{"a short word that is there matches", "fix", true},
		// A phrase is compared to the words of the card one at a time, so there
		// is nothing for it to be a typo of.
		{"a quoted phrase stays literal", `"the login page"`, true},
		{"a typo inside a quoted phrase does not match", `"the logni page"`, false},
		{"a word that is nowhere near anything", "deployment", false},
		// Every term still has to match, whether it matched exactly or not.
		{"one good term does not carry a bad one", "logni deployment", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseQuery(tt.query).Match(card, names, today); got != tt.want {
				t.Errorf("Match(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestTolerance(t *testing.T) {
	tests := []struct {
		term string
		want int
	}{
		{"", 0},
		{"ui", 0},
		{"bug", 0},
		{"bugs", 1},
		{"login", 1},
		{"release", 1},
		{"releases", 2},
		{"regression", 2},
	}
	for _, tt := range tests {
		if got := tolerance(tt.term); got != tt.want {
			t.Errorf("tolerance(%q) = %d, want %d", tt.term, got, tt.want)
		}
	}
}

func TestWithinDistance(t *testing.T) {
	tests := []struct {
		a, b string
		max  int
		want bool
	}{
		{"login", "login", 0, true},
		{"login", "logins", 0, false},
		{"login", "logins", 1, true},
		{"login", "logn", 1, true},
		{"login", "lomin", 1, true},
		{"login", "logni", 1, true},
		{"login", "ogniL", 1, false},
		{"login", "page", 1, false},
		// The length difference alone is past the limit, which is the cheap
		// answer this takes before building anything.
		{"login", "l", 2, false},
		{"", "", 0, true},
		{"", "ab", 1, false},
		// One mistyped umlaut is one edit, not the two bytes it is written in.
		{"präsentation", "prasentation", 1, true},
	}
	for _, tt := range tests {
		if got := withinDistance(tt.a, tt.b, tt.max); got != tt.want {
			t.Errorf("withinDistance(%q, %q, %d) = %v, want %v", tt.a, tt.b, tt.max, got, tt.want)
		}
	}
}

func TestSplitWords(t *testing.T) {
	got := splitWords("**Fix** the login-page (v2), it 404s!")
	want := []string{"Fix", "the", "login", "page", "v2", "it", "404s"}
	if len(got) != len(want) {
		t.Fatalf("splitWords gave %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitWords gave %q, want %q", got, want)
		}
	}
}
