package model

import "strings"

// ParseSubtasks reads the line format "flag|title", one subtask per line,
// where flag is "1" for done. It is the wire format the board page posts and
// the format the v1 schema stored. A line without "|" is an open subtask
// with the whole line as title; blank lines are skipped. IDs are not set.
func ParseSubtasks(text string) []Subtask {
	var out []Subtask
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		st := Subtask{Title: line}
		if flag, title, ok := strings.Cut(line, "|"); ok {
			st.Done = strings.TrimSpace(flag) == "1"
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

// FormatSubtasks is the inverse of ParseSubtasks.
func FormatSubtasks(subtasks []Subtask) string {
	lines := make([]string, 0, len(subtasks))
	for _, st := range subtasks {
		flag := "0"
		if st.Done {
			flag = "1"
		}
		lines = append(lines, flag+"|"+st.Title)
	}
	return strings.Join(lines, "\n")
}
