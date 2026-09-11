// Package backup turns a whole store into one JSON document and back.
//
// It is written once on top of store.Store rather than once per backend
// ([0003]), so a snapshot taken from SQLite imports into Postgres unchanged and
// every backend produces the same file for the same data. The scheduler in
// this package puts those documents somewhere off the machine on a timer; see
// [0012] for why S3 is a place snapshots go rather than a place boards live.
//
// [0003]: ../../docs/adr/0003-store-interface-and-portable-ids.md
// [0012]: ../../docs/adr/0012-snapshots-are-the-portable-format.md
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"kanban/internal/model"
	"kanban/internal/store"
)

// Format is the version of the snapshot document. Every snapshot carries it
// and Read checks it, so a file written by a later version is refused with a
// sentence instead of being half understood. Bump it when a change to the
// types below alters what Import has to do, not when a field is added that an
// older importer can ignore.
const Format = 1

const dateOnly = "2006-01-02"

// Errors Import and Read return, for callers that want to tell the cases apart.
var (
	ErrFormat  = errors.New("backup: unsupported snapshot format")
	ErrInvalid = errors.New("backup: invalid snapshot")
	ErrExists  = errors.New("backup: board already exists")
)

// Snapshot is everything in a store at one moment.
//
// Nothing here carries a position. The order of the arrays is the order of the
// board, which is one fewer thing that can contradict itself in a file people
// edit by hand, and it is what Import replays. Card and comment IDs are kept
// as they are, because they are portable by construction ([0003]) and a
// restore that renumbered them would break every link anyone had saved.
type Snapshot struct {
	Format  int       `json:"format"`
	TakenAt time.Time `json:"taken_at"`
	// Build is the version of the binary that wrote the file. Nothing reads
	// it; it is there for whoever is holding a snapshot and wondering.
	Build  string  `json:"build,omitempty"`
	Boards []Board `json:"boards"`
}

// Board and everything it owns. Cards hang off the board rather than off their
// column: an archived card still belongs to the column it left, and both store
// methods that return cards return one flat list per board.
type Board struct {
	ID     string `json:"id"`
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	Layout string `json:"layout"`
	// SLA is the response-time promise, absent for a board that has none.
	SLA       *SLA      `json:"sla,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Columns   []Column  `json:"columns"`
	Labels    []Label   `json:"labels,omitempty"`
	Cards     []Card    `json:"cards,omitempty"`
}

// Column is a lane. Its cards are in Board.Cards, keyed by ColumnID.
type Column struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	WIPLimit int    `json:"wip_limit,omitempty"`
	// StopsClock is a column the response-time clock does not run in.
	StopsClock bool `json:"stops_clock,omitempty"`
}

// SLA is a board's response-time promise, written the way the JSON API writes
// it: day names and clock readings rather than the bitmask and the minutes the
// model holds, because a snapshot is a document people read and edit.
type SLA struct {
	// ResponseHours is the promise in office hours, 0 for a board that keeps
	// its office hours on file and makes no promise against them.
	ResponseHours int `json:"response_hours,omitempty"`
	// Days are the office days, lowercase and three letters: ["mon", "tue"].
	Days []string `json:"days,omitempty"`
	// Start and End are HH:MM. An End of "00:00" is the midnight that ends the
	// day, so a desk that never closes is "00:00" to "00:00".
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
	// Zone is an IANA name such as Europe/Zurich; empty is UTC.
	Zone string `json:"zone,omitempty"`
}

// toSLA is the model's promise as a document, or nil for a board that has
// nothing to say: an older snapshot has no sla block and an importer that meets
// one gives the board the office week every new board gets.
func toSLA(s model.SLA) *SLA {
	if s == (model.SLA{}) {
		return nil
	}
	return &SLA{
		ResponseHours: s.ResponseHours,
		Days:          s.Days.Names(),
		Start:         model.ClockString(s.Start),
		End:           model.ClockString(s.End),
		Zone:          s.Zone,
	}
}

// parse reads the document back. A nil promise is the default office week,
// which is what CreateBoard would have given the board.
func (s *SLA) parse() (model.SLA, error) {
	out := model.DefaultSLA()
	if s == nil {
		return out, nil
	}
	out.ResponseHours = s.ResponseHours
	out.Zone = s.Zone
	days, err := model.ParseDays(s.Days)
	if err != nil {
		return out, err
	}
	if s.Days != nil {
		out.Days = days
	}
	if s.Start != "" {
		if out.Start, err = model.ParseClock(s.Start, false); err != nil {
			return out, fmt.Errorf("start %q: %w", s.Start, err)
		}
	}
	if s.End != "" {
		if out.End, err = model.ParseClock(s.End, true); err != nil {
			return out, fmt.Errorf("end %q: %w", s.End, err)
		}
	}
	return out, nil
}

// Label is a per-board tag.
type Label struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

// Card is one task. Live cards come first in Board.Cards, in board order;
// archived ones follow, most recently archived first.
type Card struct {
	ID          string `json:"id"`
	ColumnID    string `json:"column_id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	DueDate     string `json:"due_date,omitempty"` // YYYY-MM-DD
	Assignee    string `json:"assignee,omitempty"`
	// ArchivedAt is absent while the card is on the board.
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	Labels     []string   `json:"labels,omitempty"`
	Subtasks   []Subtask  `json:"subtasks,omitempty"`
	Comments   []Comment  `json:"comments,omitempty"`
}

// Subtask is one checklist item.
type Subtask struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Done  bool   `json:"done,omitempty"`
}

// Comment is one message on a card.
type Comment struct {
	ID        string    `json:"id"`
	Author    string    `json:"author,omitempty"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Export reads the whole store into a snapshot.
//
// It holds the result in memory rather than streaming it, because Import needs
// the document checked before it writes anything, and a board that does not fit
// in the memory of the process that serves it could not be drawn either.
//
// The cost is one call per board plus one per card that carries comments, which
// is more than a page would ever do. A snapshot is taken by a command line or
// by a timer, not by a request.
func Export(ctx context.Context, s store.Store) (*Snapshot, error) {
	boards, err := s.ListBoards(ctx)
	if err != nil {
		return nil, fmt.Errorf("list boards: %w", err)
	}
	snap := &Snapshot{Format: Format, TakenAt: time.Now().UTC(), Boards: make([]Board, 0, len(boards))}
	for _, b := range boards {
		out := Board{
			ID:        string(b.ID),
			Slug:      b.Slug,
			Name:      b.Name,
			Layout:    model.LayoutOrDefault(b.Layout),
			SLA:       toSLA(b.SLA),
			CreatedAt: b.CreatedAt.UTC(),
			UpdatedAt: b.UpdatedAt.UTC(),
		}
		for _, c := range b.Columns {
			out.Columns = append(out.Columns, Column{
				ID: string(c.ID), Name: c.Name, WIPLimit: c.WIPLimit, StopsClock: c.StopsClock,
			})
		}
		for _, l := range b.Labels {
			out.Labels = append(out.Labels, Label{ID: string(l.ID), Name: l.Name, Color: l.Color})
		}

		live, err := s.ListCards(ctx, b.ID)
		if err != nil {
			return nil, fmt.Errorf("list cards of %s: %w", b.Slug, err)
		}
		archived, err := s.ListArchivedCards(ctx, b.ID)
		if err != nil {
			return nil, fmt.Errorf("list archived cards of %s: %w", b.Slug, err)
		}
		// One call for the whole board instead of a thread read per card. A
		// board where nobody has commented then costs nothing extra.
		counts, err := s.CountComments(ctx, b.ID)
		if err != nil {
			return nil, fmt.Errorf("count comments of %s: %w", b.Slug, err)
		}
		for _, group := range [][]model.Card{live, archived} {
			for _, c := range group {
				card, err := exportCard(ctx, s, c, counts[c.ID] > 0)
				if err != nil {
					return nil, err
				}
				out.Cards = append(out.Cards, card)
			}
		}
		snap.Boards = append(snap.Boards, out)
	}
	return snap, nil
}

// exportCard turns one card into its snapshot shape. Its comments are read only
// when this card has any, which the caller knows from one count taken across the
// whole board, so a board nobody commented on costs no reads at all.
func exportCard(ctx context.Context, s store.Store, c model.Card, hasComments bool) (Card, error) {
	out := Card{
		ID:          string(c.ID),
		ColumnID:    string(c.ColumnID),
		Title:       c.Title,
		Description: c.Description,
		Assignee:    c.Assignee,
		CreatedAt:   c.CreatedAt.UTC(),
		UpdatedAt:   c.UpdatedAt.UTC(),
	}
	if !c.DueDate.IsZero() {
		out.DueDate = c.DueDate.UTC().Format(dateOnly)
	}
	if c.Archived() {
		at := c.ArchivedAt.UTC()
		out.ArchivedAt = &at
	}
	for _, id := range c.Labels {
		out.Labels = append(out.Labels, string(id))
	}
	for _, st := range c.Subtasks {
		out.Subtasks = append(out.Subtasks, Subtask{ID: string(st.ID), Title: st.Title, Done: st.Done})
	}
	if !hasComments {
		return out, nil
	}
	comments, err := s.ListComments(ctx, c.ID)
	if err != nil {
		return out, fmt.Errorf("list comments of card %s: %w", c.ID, err)
	}
	for _, m := range comments {
		out.Comments = append(out.Comments, Comment{
			ID:        string(m.ID),
			Author:    m.Author,
			Body:      m.Body,
			CreatedAt: m.CreatedAt.UTC(),
		})
	}
	return out, nil
}

// Write encodes a snapshot as indented JSON with a trailing newline. Indented,
// because a snapshot is read and diffed by people as often as it is imported.
// Nothing compresses it on the way out, so the whitespace is paid for in full;
// a board's worth of JSON is small enough that legibility is the better buy.
func Write(w io.Writer, snap *Snapshot) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// Nothing in a card is HTML, and the escaping would turn every < in a
	// description into < in a file people read.
	enc.SetEscapeHTML(false)
	return enc.Encode(snap)
}

// Read decodes a snapshot and refuses a format this build does not know.
func Read(r io.Reader) (*Snapshot, error) {
	var snap Snapshot
	if err := json.NewDecoder(r).Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	switch {
	case snap.Format == 0:
		return nil, fmt.Errorf("%w: no format field; this is not a kanban snapshot", ErrFormat)
	case snap.Format > Format:
		return nil, fmt.Errorf("%w: file is format %d, this build reads up to %d", ErrFormat, snap.Format, Format)
	}
	return &snap, nil
}

// Bytes is Write into a buffer, for callers that hand the document to a Target.
func Bytes(snap *Snapshot) ([]byte, error) {
	var buf bytes.Buffer
	if err := Write(&buf, snap); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
