// Package service holds the use cases. One method per user action; the HTMX
// handlers and, later, the JSON API call these and nothing else. Validation,
// ID generation, timestamps and WIP limits live here, so every backend and
// every front-end gets the same rules.
package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"kanban/internal/model"
	"kanban/internal/store"
)

// Limits on user input, in characters.
const (
	MaxTitle       = 200
	MaxName        = 100
	MaxDescription = 20000
	// MaxAssignee is generous for an address; the cap is there so a form
	// post cannot put a novel in the column.
	MaxAssignee = 320
	// MaxSubtasks caps the checklist. Without it one request can store a card
	// that renders to tens of megabytes, which the pod cannot hold in memory,
	// and the card stays in the database so every later render fails too.
	MaxSubtasks = 100
	MaxSlug     = 64
)

// Default board created on an empty database.
const (
	DefaultBoardSlug = "board"
	DefaultBoardName = "Kanban Board"
)

// DefaultColumns are the columns of a board created without an explicit list.
var DefaultColumns = []string{"To Do", "In Progress", "Done"}

// ErrWIPLimit is returned when a move or create would exceed a column's limit.
var ErrWIPLimit = errors.New("column is at its WIP limit")

// ValidationError names the offending field. Handlers turn it into a 400.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

func invalid(field, msg string) error { return &ValidationError{Field: field, Message: msg} }

// Kanban is the application service.
type Kanban struct {
	store store.Store
	now   func() time.Time
	newID func() model.ID
}

// Option configures New.
type Option func(*Kanban)

// WithClock replaces time.Now; tests use it.
func WithClock(now func() time.Time) Option { return func(k *Kanban) { k.now = now } }

// WithIDs replaces the ID generator; tests use it.
func WithIDs(newID func() model.ID) Option { return func(k *Kanban) { k.newID = newID } }

// New wires the service to a store.
func New(s store.Store, opts ...Option) *Kanban {
	k := &Kanban{store: s, now: func() time.Time { return time.Now().UTC() }, newID: model.NewID}
	for _, o := range opts {
		o(k)
	}
	return k
}

var slugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Slugify turns a name into a URL slug: lower case, runs of anything but
// letters and digits become one hyphen. The result may be empty.
func Slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if utf8.RuneCountInString(s) > MaxSlug {
		s = strings.TrimRight(s[:MaxSlug], "-")
	}
	return s
}

func checkText(field, value string, max int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return invalid(field, "must not be empty")
	}
	if utf8.RuneCountInString(value) > max {
		return invalid(field, fmt.Sprintf("must be at most %d characters", max))
	}
	return nil
}

// --- boards -----------------------------------------------------------------

func (k *Kanban) Boards(ctx context.Context) ([]model.Board, error) {
	return k.store.ListBoards(ctx)
}

func (k *Kanban) Board(ctx context.Context, slug string) (*model.Board, error) {
	return k.store.GetBoard(ctx, slug)
}

func (k *Kanban) BoardByID(ctx context.Context, id model.ID) (*model.Board, error) {
	return k.store.GetBoardByID(ctx, id)
}

// EnsureDefaultBoard creates the default board when the store has none and
// returns the boards afterwards. serve calls it on start so that a fresh
// database shows a usable board.
func (k *Kanban) EnsureDefaultBoard(ctx context.Context) ([]model.Board, error) {
	boards, err := k.store.ListBoards(ctx)
	if err != nil {
		return nil, err
	}
	if len(boards) > 0 {
		return boards, nil
	}
	b, err := k.CreateBoard(ctx, DefaultBoardName, DefaultBoardSlug, nil)
	if err != nil {
		return nil, err
	}
	return []model.Board{*b}, nil
}

// CreateBoard creates a board. An empty slug is derived from the name; an
// empty column list means DefaultColumns.
func (k *Kanban) CreateBoard(ctx context.Context, name, slug string, columns []string) (*model.Board, error) {
	name = strings.TrimSpace(name)
	if err := checkText("name", name, MaxName, true); err != nil {
		return nil, err
	}
	if slug == "" {
		slug = Slugify(name)
	}
	if !slugRe.MatchString(slug) || len(slug) > MaxSlug {
		return nil, invalid("slug", "must be lower-case letters, digits and single hyphens")
	}
	if len(columns) == 0 {
		columns = DefaultColumns
	}
	now := k.now()
	b := &model.Board{ID: k.newID(), Slug: slug, Name: name, CreatedAt: now, UpdatedAt: now}
	for _, c := range columns {
		c = strings.TrimSpace(c)
		if err := checkText("column", c, MaxName, true); err != nil {
			return nil, err
		}
		b.Columns = append(b.Columns, model.Column{ID: k.newID(), BoardID: b.ID, Name: c})
	}
	if err := k.store.CreateBoard(ctx, b); err != nil {
		return nil, err
	}
	return b, nil
}

// RenameBoard changes name and slug.
func (k *Kanban) RenameBoard(ctx context.Context, id model.ID, name, slug string) (*model.Board, error) {
	b, err := k.store.GetBoardByID(ctx, id)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if err := checkText("name", name, MaxName, true); err != nil {
		return nil, err
	}
	if slug == "" {
		slug = b.Slug
	}
	if !slugRe.MatchString(slug) || len(slug) > MaxSlug {
		return nil, invalid("slug", "must be lower-case letters, digits and single hyphens")
	}
	b.Name, b.Slug, b.UpdatedAt = name, slug, k.now()
	if err := k.store.UpdateBoard(ctx, b); err != nil {
		return nil, err
	}
	return b, nil
}

func (k *Kanban) DeleteBoard(ctx context.Context, id model.ID) error {
	return k.store.DeleteBoard(ctx, id)
}

// --- columns ----------------------------------------------------------------

func (k *Kanban) AddColumn(ctx context.Context, boardID model.ID, name string, wipLimit int) (*model.Column, error) {
	name = strings.TrimSpace(name)
	if err := checkText("name", name, MaxName, true); err != nil {
		return nil, err
	}
	if wipLimit < 0 {
		return nil, invalid("wip_limit", "must not be negative")
	}
	c := &model.Column{ID: k.newID(), BoardID: boardID, Name: name, WIPLimit: wipLimit}
	if err := k.store.CreateColumn(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (k *Kanban) UpdateColumn(ctx context.Context, id model.ID, name string, wipLimit int) error {
	name = strings.TrimSpace(name)
	if err := checkText("name", name, MaxName, true); err != nil {
		return err
	}
	if wipLimit < 0 {
		return invalid("wip_limit", "must not be negative")
	}
	return k.store.UpdateColumn(ctx, &model.Column{ID: id, Name: name, WIPLimit: wipLimit})
}

// RemoveColumn deletes a column, moving its cards to moveCardsTo when given.
func (k *Kanban) RemoveColumn(ctx context.Context, id, moveCardsTo model.ID) error {
	return k.store.DeleteColumn(ctx, id, moveCardsTo)
}

func (k *Kanban) ReorderColumns(ctx context.Context, boardID model.ID, order []model.ID) error {
	return k.store.ReorderColumns(ctx, boardID, order)
}

// --- cards ------------------------------------------------------------------

// CardInput is what a user can set on a card.
type CardInput struct {
	Title       string
	Description string
	DueDate     string // YYYY-MM-DD or empty
	// Assignee is an address, empty to unassign. Not checked against a user
	// list, because there is none: whoever the identity provider let in is
	// who can be named.
	Assignee string
	Labels   []model.ID
	Subtasks []model.Subtask // IDs may be empty for new ones
}

func (k *Kanban) applyInput(c *model.Card, in CardInput) error {
	title := strings.TrimSpace(in.Title)
	if err := checkText("title", title, MaxTitle, true); err != nil {
		return err
	}
	if err := checkText("description", in.Description, MaxDescription, false); err != nil {
		return err
	}
	if len(in.Subtasks) > MaxSubtasks {
		return invalid("subtasks", fmt.Sprintf("at most %d subtasks", MaxSubtasks))
	}
	assignee := strings.TrimSpace(in.Assignee)
	if err := checkText("assignee", assignee, MaxAssignee, false); err != nil {
		return err
	}
	due := time.Time{}
	if in.DueDate != "" {
		d, err := time.Parse("2006-01-02", in.DueDate)
		if err != nil {
			return invalid("due_date", "must be YYYY-MM-DD")
		}
		due = d
	}
	var subtasks []model.Subtask
	for _, st := range in.Subtasks {
		st.Title = strings.TrimSpace(st.Title)
		if st.Title == "" {
			continue
		}
		if err := checkText("subtask", st.Title, MaxTitle, true); err != nil {
			return err
		}
		if st.ID == "" {
			st.ID = k.newID()
		}
		st.Position = len(subtasks) + 1
		subtasks = append(subtasks, st)
	}
	c.Title, c.Description, c.DueDate, c.Assignee = title, strings.ReplaceAll(in.Description, "\r\n", "\n"), due, assignee
	c.Labels = append([]model.ID(nil), in.Labels...)
	c.Subtasks = subtasks
	return nil
}

// Cards returns a board's cards, ordered by column then position.
func (k *Kanban) Cards(ctx context.Context, boardID model.ID) ([]model.Card, error) {
	return k.store.ListCards(ctx, boardID)
}

func (k *Kanban) Card(ctx context.Context, id model.ID) (*model.Card, error) {
	return k.store.GetCard(ctx, id)
}

// wipRoom reports whether column can hold `adding` more cards.
func (k *Kanban) wipRoom(ctx context.Context, board *model.Board, columnID model.ID, adding int, ignore map[model.ID]bool) error {
	col := board.Column(columnID)
	if col == nil {
		return store.ErrNotFound
	}
	if col.WIPLimit == 0 {
		return nil
	}
	cards, err := k.store.ListCards(ctx, board.ID)
	if err != nil {
		return err
	}
	n := adding
	for _, c := range cards {
		if c.ColumnID == columnID && !ignore[c.ID] {
			n++
		}
	}
	if n > col.WIPLimit {
		return ErrWIPLimit
	}
	return nil
}

// CreateCard appends a card to a column.
func (k *Kanban) CreateCard(ctx context.Context, boardID, columnID model.ID, in CardInput) (*model.Card, error) {
	board, err := k.store.GetBoardByID(ctx, boardID)
	if err != nil {
		return nil, err
	}
	if err := k.wipRoom(ctx, board, columnID, 1, nil); err != nil {
		return nil, err
	}
	now := k.now()
	c := &model.Card{ID: k.newID(), BoardID: boardID, ColumnID: columnID, CreatedAt: now, UpdatedAt: now}
	if err := k.applyInput(c, in); err != nil {
		return nil, err
	}
	if err := k.store.CreateCard(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// UpdateCard replaces a card's content; column and position are untouched.
func (k *Kanban) UpdateCard(ctx context.Context, id model.ID, in CardInput) (*model.Card, error) {
	c, err := k.store.GetCard(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := k.applyInput(c, in); err != nil {
		return nil, err
	}
	c.UpdatedAt = k.now()
	if err := k.store.UpdateCard(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// ArchiveCard takes a card off the board. It keeps its column, position,
// labels and subtasks, so RestoreCard puts back the same card.
func (k *Kanban) ArchiveCard(ctx context.Context, id model.ID) error {
	return k.store.SetCardArchived(ctx, id, k.now())
}

// RestoreCard puts an archived card back on the board, in the column it left.
//
// It does not check the destination's WIP limit. A limit is there to stop new
// work being started, and restoring is undoing a mistake; refusing it would
// leave the card in the archive with nothing the user can do about it except
// move something else first.
func (k *Kanban) RestoreCard(ctx context.Context, id model.ID) error {
	return k.store.SetCardArchived(ctx, id, time.Time{})
}

// ArchivedCards returns a board's archived cards, most recently archived first.
func (k *Kanban) ArchivedCards(ctx context.Context, boardID model.ID) ([]model.Card, error) {
	return k.store.ListArchivedCards(ctx, boardID)
}

func (k *Kanban) DeleteCard(ctx context.Context, id model.ID) error {
	return k.store.DeleteCard(ctx, id)
}

// ReorderCards sets the authoritative order of one column, moving listed
// cards in from other columns. It refuses to exceed the column's WIP limit.
func (k *Kanban) ReorderCards(ctx context.Context, boardID, columnID model.ID, order []model.ID) error {
	board, err := k.store.GetBoardByID(ctx, boardID)
	if err != nil {
		return err
	}
	ignore := map[model.ID]bool{}
	for _, id := range order {
		ignore[id] = true
	}
	if err := k.wipRoom(ctx, board, columnID, len(order), ignore); err != nil {
		return err
	}
	return k.store.ReorderCards(ctx, boardID, columnID, order)
}

// --- labels -----------------------------------------------------------------

func (k *Kanban) CreateLabel(ctx context.Context, boardID model.ID, name, color string) (*model.Label, error) {
	name = strings.TrimSpace(name)
	if err := checkText("name", name, MaxName, true); err != nil {
		return nil, err
	}
	l := &model.Label{ID: k.newID(), BoardID: boardID, Name: name, Color: strings.TrimSpace(color)}
	if err := k.store.CreateLabel(ctx, l); err != nil {
		return nil, err
	}
	return l, nil
}

func (k *Kanban) UpdateLabel(ctx context.Context, id model.ID, name, color string) error {
	name = strings.TrimSpace(name)
	if err := checkText("name", name, MaxName, true); err != nil {
		return err
	}
	return k.store.UpdateLabel(ctx, &model.Label{ID: id, Name: name, Color: strings.TrimSpace(color)})
}

func (k *Kanban) DeleteLabel(ctx context.Context, id model.ID) error {
	return k.store.DeleteLabel(ctx, id)
}

// BulkAction is one of the operations Bulk applies to a set of cards.
type BulkAction string

const (
	BulkDelete  BulkAction = "delete"
	BulkMove    BulkAction = "move"
	BulkAssign  BulkAction = "assign"
	BulkArchive BulkAction = "archive"
)

// MaxBulk caps one bulk request. Every card is a separate store call, so an
// unbounded list would hold a request open for as long as someone cared to
// make it.
const MaxBulk = 200

// BulkResult reports what a bulk request did. Failures are collected rather
// than aborting, because the operations are independent: a card someone else
// deleted a second ago should not stop the other forty-nine from moving.
type BulkResult struct {
	Changed []model.ID
	Failed  map[model.ID]error
}

// Bulk applies action to ids. target is the destination column for BulkMove
// and the address for BulkAssign; it is ignored for BulkDelete.
//
// There is no transaction across the set, and there deliberately is not one:
// the store interface is one method per user action so that backends without
// transactions can implement it, and a bulk operation that needed atomicity
// would leak that requirement into every backend. Partial success is
// reported instead of hidden.
func (k *Kanban) Bulk(ctx context.Context, boardID model.ID, action BulkAction, ids []model.ID, target string) (BulkResult, error) {
	res := BulkResult{Failed: map[model.ID]error{}}
	if len(ids) == 0 {
		return res, invalid("ids", "no cards selected")
	}
	if len(ids) > MaxBulk {
		return res, invalid("ids", fmt.Sprintf("at most %d cards at a time", MaxBulk))
	}
	switch action {
	case BulkDelete, BulkMove, BulkAssign, BulkArchive:
	default:
		return res, invalid("action", "unknown action")
	}

	seen := map[model.ID]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true

		// Every card is re-read and checked against the board in the request,
		// so a crafted id cannot reach a card on someone else's board.
		c, err := k.store.GetCard(ctx, id)
		if err != nil {
			res.Failed[id] = err
			continue
		}
		if c.BoardID != boardID {
			res.Failed[id] = store.ErrNotFound
			continue
		}

		switch action {
		case BulkDelete:
			err = k.store.DeleteCard(ctx, id)
		case BulkArchive:
			err = k.store.SetCardArchived(ctx, id, k.now())
		case BulkAssign:
			c.Assignee = strings.TrimSpace(target)
			c.UpdatedAt = k.now()
			err = k.store.UpdateCard(ctx, c)
		case BulkMove:
			err = k.store.ReorderCards(ctx, boardID, model.ID(target), []model.ID{id})
		}
		if err != nil {
			res.Failed[id] = err
			continue
		}
		res.Changed = append(res.Changed, id)
	}
	return res, nil
}
