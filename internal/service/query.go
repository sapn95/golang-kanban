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
	// Text matches title, description or a label name, case-insensitively, and
	// a word that matches none of them literally is tried once more as a typo.
	// Every term has to match, so adding a word narrows.
	//
	// Label names are in there because that is what people expect: a label is
	// something you can see on the card, and a word you can see on a card
	// should find it. label: is still the way to say "only the label", which
	// is what you want when the same word is also in a title.
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
	return q.matchesText(c, labelNames) &&
		q.matchesLabels(c, labelNames) &&
		q.matchesAssignee(c) &&
		q.matchesDue(c, today)
}

// matchesText requires every bare word, so adding one narrows. A word matches
// the title, the description or the name of a label the card carries.
//
// A term that is nowhere to be found as it was typed is tried again as a typo,
// against the words of the same three fields. That is the last thing attempted
// rather than the first, so an exact match is never diluted by a near one, and
// the words are only split when a term actually needs them: a search that finds
// what it asked for costs no more than it did before.
func (q Query) matchesText(c model.Card, labelNames map[model.ID]string) bool {
	title, description := strings.ToLower(c.Title), strings.ToLower(c.Description)
	var words []string
	for _, term := range q.Text {
		if strings.Contains(title, term) || strings.Contains(description, term) {
			continue
		}
		if q.labelContains(c, labelNames, term) {
			continue
		}
		if words == nil {
			words = cardWords(title, description, c, labelNames)
		}
		if !closeToAny(term, words) {
			return false
		}
	}
	return true
}

// labelContains reports whether any of a card's labels has the term in its
// name, so label:bug finds "bug" and "bugfix".
func (q Query) labelContains(c model.Card, labelNames map[model.ID]string, term string) bool {
	for _, id := range c.Labels {
		if strings.Contains(labelNames[id], term) {
			return true
		}
	}
	return false
}

// matchesLabels requires every label: name exactly. Naming a label is how you
// say you mean that one, and not everything it happens to be a substring of.
func (q Query) matchesLabels(c model.Card, labelNames map[model.ID]string) bool {
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
	return true
}

// matchesAssignee tests a card against assignee:. An empty term matches
// everything, "none" matches the unassigned, and anything else is a substring
// of the address.
func (q Query) matchesAssignee(c model.Card) bool {
	switch q.Assignee {
	case "":
		return true
	case "none":
		return c.Assignee == ""
	default:
		return strings.Contains(strings.ToLower(c.Assignee), q.Assignee)
	}
}

// matchesDue tests a card against due:, which takes none, overdue, today,
// week, or a date.
func (q Query) matchesDue(c model.Card, today time.Time) bool {
	switch q.Due {
	case "none":
		return c.DueDate.IsZero()
	case "overdue":
		return !c.DueDate.IsZero() && c.DueDate.Before(today)
	case "today":
		return c.DueDate.Equal(today)
	case "week":
		// Today through the next seven days, so "this week" includes today
		// and excludes anything already overdue.
		return !c.DueDate.IsZero() && !c.DueDate.Before(today) && !c.DueDate.After(today.AddDate(0, 0, 7))
	default:
		return true
	}
}
