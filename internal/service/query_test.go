package service

import (
	"reflect"
	"testing"
	"time"

	"kanban/internal/model"
)

func TestParseQuery(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Query
	}{
		{"empty", "", Query{}},
		{"bare words are text", "fix the login", Query{Text: []string{"fix", "the", "login"}}},
		{"text is lowercased", "FIX Login", Query{Text: []string{"fix", "login"}}},
		{"a quoted phrase stays one term", `"the login page" fix`, Query{Text: []string{"the login page", "fix"}}},
		{"an unclosed quote still parses, so typing works", `"the login`, Query{Text: []string{"the login"}}},
		{"is:archived", "is:archived", Query{Archived: true}},
		{"labels accumulate", "label:bug label:ui", Query{Labels: []string{"bug", "ui"}}},
		{"assignee", "assignee:someone@example.com", Query{Assignee: "someone@example.com"}},
		{"due keywords", "due:overdue", Query{Due: "overdue"}},
		{"mixed", `is:archived label:bug login`, Query{Text: []string{"login"}, Labels: []string{"bug"}, Archived: true}},

		// A colon in a card title is ordinary. Refusing to search for it would
		// be worse than searching for it literally.
		{"an unknown prefix is text", "ticket:1234", Query{Text: []string{"ticket:1234"}}},
		{"an unknown is: value is text", "is:blue", Query{Text: []string{"is:blue"}}},
		{"an unknown due: value is text", "due:soon", Query{Text: []string{"due:soon"}}},
		{"a bare colon is text", "note:", Query{Text: []string{"note:"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseQuery(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseQuery(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestQueryEmpty(t *testing.T) {
	if !ParseQuery("   ").Empty() {
		t.Error("whitespace is not an empty query")
	}
	if ParseQuery("is:archived").Empty() {
		t.Error("is:archived reports itself as filtering nothing")
	}
}

func TestQueryMatch(t *testing.T) {
	today := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	bug, ui := model.ID("l-bug"), model.ID("l-ui")
	names := map[model.ID]string{bug: "bug", ui: "ui"}

	card := model.Card{
		Title:       "Fix the Login page",
		Description: "It throws on submit",
		Labels:      []model.ID{bug},
		Assignee:    "Someone@Example.com",
		DueDate:     today.AddDate(0, 0, 2),
	}

	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"a term in the title", "login", true},
		{"case is ignored", "LOGIN", true},
		{"a term in the description", "submit", true},
		{"every term has to match", "login missing", false},
		{"a quoted phrase", `"the login page"`, true},
		{"a phrase that is not there", `"login form"`, false},
		{"a label the card has", "label:bug", true},
		{"a label it does not", "label:ui", false},
		{"all labels are required", "label:bug label:ui", false},
		{"assignee substring", "assignee:someone", true},
		{"assignee case is ignored", "assignee:SOMEONE@example.com", true},
		{"a different assignee", "assignee:other", false},
		{"due this week", "due:week", true},
		{"not overdue", "due:overdue", false},
		{"not due today", "due:today", false},
		{"has a due date", "due:none", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseQuery(tt.query).Match(card, names, today); got != tt.want {
				t.Errorf("%q matched %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestQueryMatchEdges(t *testing.T) {
	today := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	names := map[model.ID]string{}

	unassigned := model.Card{Title: "nobody"}
	if !ParseQuery("assignee:none").Match(unassigned, names, today) {
		t.Error("assignee:none did not match an unassigned card")
	}
	if ParseQuery("assignee:none").Match(model.Card{Assignee: "a@b"}, names, today) {
		t.Error("assignee:none matched an assigned card")
	}
	if !ParseQuery("due:none").Match(unassigned, names, today) {
		t.Error("due:none did not match a card with no due date")
	}

	overdue := model.Card{Title: "late", DueDate: today.AddDate(0, 0, -1)}
	if !ParseQuery("due:overdue").Match(overdue, names, today) {
		t.Error("due:overdue did not match yesterday")
	}
	// Overdue is not "this week": a card that is already late should not hide
	// among the ones that are merely coming up.
	if ParseQuery("due:week").Match(overdue, names, today) {
		t.Error("due:week matched a card that is already overdue")
	}
	if !ParseQuery("due:today").Match(model.Card{DueDate: today}, names, today) {
		t.Error("due:today did not match today")
	}
	if !ParseQuery("due:week").Match(model.Card{DueDate: today}, names, today) {
		t.Error("due:week excluded today")
	}
}
