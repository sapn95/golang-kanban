package model

import "strings"

// ParseSubtasks reads the line format the board page posts, one subtask per
// line, and is also the format the v1 schema stored.
//
// A line is "id|flag|title", where flag is "1" for done. The id is what keeps a
// checklist's lines the same lines across a save: the edit form used to post
// only the flag and the title, so every save minted fresh ids, and a tick
// queued on somebody else's phone came back 404 against a line that had not
// visibly changed. An empty id is a line somebody has just typed, and the
// service gives it one.
//
// Two older shapes still parse, because they are what the v1 rows hold: the
// two-field "flag|title", and a bare line, which is an open subtask with the
// whole line as its title.
//
// Telling the two apart matters, because a v1 title is allowed to contain a
// pipe: "0|pipe|open" is one v1 subtask called "pipe|open" and not an id of
// "0". A line is read as the three-field shape only when the middle field is
// exactly "0" or "1" and the first is not, which no v1 flag can satisfy and
// every generated id can. Blank lines are skipped, as is a line whose title is
// empty once trimmed.
func ParseSubtasks(text string) []Subtask {
	var out []Subtask
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		st := Subtask{Title: line}
		if first, rest, ok := strings.Cut(line, "|"); ok {
			id, flag := strings.TrimSpace(first), ""
			title := rest
			if mid, after, ok := strings.Cut(rest, "|"); ok && isFlag(mid) && !isFlag(id) {
				flag, title = strings.TrimSpace(mid), after
				st.ID = ID(id)
			} else {
				flag = id
			}
			st.Done = flag == "1"
			st.Title = strings.TrimSpace(title)
			if st.Title == "" {
				continue
			}
		}
		st.Position = len(out) + 1
		out = append(out, st)
	}
	return out
}

// isFlag reports whether a field is a done flag, which is the one test that
// tells the three-field shape from a two-field line whose title has a pipe in
// it.
func isFlag(s string) bool {
	s = strings.TrimSpace(s)
	return s == "0" || s == "1"
}
