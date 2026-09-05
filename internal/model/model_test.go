package model

import (
	"regexp"
	"testing"
)

func TestNewID(t *testing.T) {
	re := regexp.MustCompile(`^[a-z2-7]{26}$`)
	seen := map[ID]bool{}
	for i := 0; i < 100; i++ {
		id := NewID()
		if !re.MatchString(string(id)) {
			t.Fatalf("bad id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestSubtasks(t *testing.T) {
	got := ParseSubtasks("1|done thing\r\n0|open thing\n\n  |  \nno pipe\n1|a|b\n")
	want := []Subtask{
		{Title: "done thing", Done: true, Position: 1},
		{Title: "open thing", Position: 2},
		{Title: "no pipe", Position: 3},
		{Title: "a|b", Done: true, Position: 4},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %+v want %+v", i, got[i], want[i])
		}
	}
	if s := FormatSubtasks(want); s != "1|done thing\n0|open thing\n0|no pipe\n1|a|b" {
		t.Errorf("FormatSubtasks = %q", s)
	}
	if ParseSubtasks("") != nil {
		t.Error("empty input should give nil")
	}
}

func TestBoardLookups(t *testing.T) {
	b := &Board{Columns: []Column{{ID: "c1"}}, Labels: []Label{{ID: "l1"}}}
	if b.Column("c1") == nil || b.Column("x") != nil || b.Label("l1") == nil || b.Label("x") != nil {
		t.Error("lookups wrong")
	}
}
