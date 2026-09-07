package service

import (
	"strings"
	"time"

	"kanban/internal/model"
)

// Query is a parsed search. The zero Query matches every card on the board.
//
// Filtering happens here rather than in SQL on purpose. The store interface is
// coarse by design so that a backend without a query language can implement it,
// and pushing a search syntax into three backends would triple both the surface
// and the places a filter can disagree with itself. A board holds hundreds of
// cards, not millions; loading them and filtering in one place is cheap and
// gives every backend identical behaviour for free.
type Query struct {
	// Text matches title or description, case-insensitively. Every term has
	// to match, so adding a word narrows.
	Text []string
	// Labels are label names; a card must carry all of them.
	Labels []string
	// Assignee matches a substring of the address. "none" means unassigned.
	Assignee string
	// Due is one of "", "none", "overdue", "today", "week".
	Due string
	// Archived searches the archive instead of the board.
	Archived bool
}

// Empty reports whether the query would filter nothing out.
func (q Query) Empty() bool {
	return len(q.Text) == 0 && len(q.Labels) == 0 && q.Assignee == "" && q.Due == "" && !q.Archived
}

// ParseQuery reads a search string.
//
// Bare words match title and description. A word may be quoted to keep spaces
// in it. The prefixes is:, label:, assignee: and due: filter instead, and an
// unrecognised prefix is treated as text rather than rejected, because a colon
// in a card title is ordinary and refusing to search for it would be worse than
// searching for it literally.
func ParseQuery(s string) Query {
	var q Query
	for _, tok := range tokenise(s) {
		key, value, ok := strings.Cut(tok, ":")
		if !ok || value == "" {
			q.Text = append(q.Text, strings.ToLower(tok))
			continue
		}
		switch strings.ToLower(key) {
		case "is":
			if strings.EqualFold(value, "archived") {
				q.Archived = true
			} else {
				q.Text = append(q.Text, strings.ToLower(tok))
			}
		case "label":
			q.Labels = append(q.Labels, strings.ToLower(value))
		case "assignee":
			q.Assignee = strings.ToLower(value)
		case "due":
			switch strings.ToLower(value) {
			case "none", "overdue", "today", "week":
				q.Due = strings.ToLower(value)
			default:
				q.Text = append(q.Text, strings.ToLower(tok))
			}
		default:
			q.Text = append(q.Text, strings.ToLower(tok))
		}
	}
	return q
}

// tokenise splits on whitespace, keeping "quoted phrases" together. A trailing
// quote is not required, so a query is usable while it is still being typed.
func tokenise(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case !inQuote && (r == ' ' || r == '\t' || r == '\n'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// Match reports whether a card satisfies the query. labelNames maps a label id
// to its lowercased name; the caller resolves them once for the whole board
// rather than per card.
func (q Query) Match(c model.Card, labelNames map[model.ID]string, today time.Time) bool {
	for _, term := range q.Text {
		if !strings.Contains(strings.ToLower(c.Title), term) &&
			!strings.Contains(strings.ToLower(c.Description), term) {
			return false
		}
	}

	for _, want := range q.Labels {
		found := false
		for _, id := range c.Labels {
			if labelNames[id] == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	if q.Assignee != "" {
		if q.Assignee == "none" {
			if c.Assignee != "" {
				return false
			}
		} else if !strings.Contains(strings.ToLower(c.Assignee), q.Assignee) {
			return false
		}
	}

	switch q.Due {
	case "none":
		if !c.DueDate.IsZero() {
			return false
		}
	case "overdue":
		if c.DueDate.IsZero() || !c.DueDate.Before(today) {
			return false
		}
	case "today":
		if !c.DueDate.Equal(today) {
			return false
		}
	case "week":
		// Today through the next seven days, so "this week" includes today
		// and excludes anything already overdue.
		if c.DueDate.IsZero() || c.DueDate.Before(today) || c.DueDate.After(today.AddDate(0, 0, 7)) {
			return false
		}
	}

	return true
}
