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
	"slices"
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
	// MaxComment is a paragraph or two. A comment cannot be edited, so the cap
	// is also the point at which someone should have written a card instead.
	MaxComment = 5000
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

// ErrNotAuthor is returned when someone tries to remove a comment they did not
// write. It is separate from a validation error because the request is well
// formed; it is the person making it who is wrong.
var ErrNotAuthor = errors.New("only the author can remove a comment")

// ValidationError names the offending field. Handlers turn it into a 400.
type ValidationError struct {
	Field   string
	Message string
}

// Error prints the field and what is wrong with it, which is what a form shows
// beside the input and what the JSON API puts in its body.
func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

// invalid builds a ValidationError. Short because it is written on nearly every
// guard below.
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

// checkText enforces the two rules every text field here has: it must not be
// empty when it is required, and it must not be longer than the column holding
// it. Checked before the store, so the message names the field.
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

// Boards returns every board, for the list and the switcher.
func (k *Kanban) Boards(ctx context.Context) ([]model.Board, error) {
	return k.store.ListBoards(ctx)
}

// Board returns one board by slug, which is what a URL carries.
func (k *Kanban) Board(ctx context.Context, slug string) (*model.Board, error) {
	return k.store.GetBoard(ctx, slug)
}

// BoardByID returns one board by id, which is what a card carries.
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
	// DefaultSLA is the office week with the clock switched off, the same thing
	// the SQL column defaults give a board that predates the promise. Set here
	// rather than left zero, so the settings form opens on office hours somebody
	// would want on every backend, the in-memory one included.
	b := &model.Board{ID: k.newID(), Slug: slug, Name: name, Layout: model.LayoutColumns,
		SLA: model.DefaultSLA(), CreatedAt: now, UpdatedAt: now}
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

// SetBoardLayout changes how the board draws its columns. It is a property of
// the board rather than of the viewer, because a board that reads better as
// rows reads better as rows for everyone looking at it.
func (k *Kanban) SetBoardLayout(ctx context.Context, id model.ID, layout string) error {
	switch layout {
	case model.LayoutColumns, model.LayoutRows:
	default:
		return invalid("layout", "must be columns or rows")
	}
	b, err := k.store.GetBoardByID(ctx, id)
	if err != nil {
		return err
	}
	b.Layout, b.UpdatedAt = layout, k.now()
	return k.store.UpdateBoard(ctx, b)
}

// SetBoardSLA sets the board's response-time promise. Zero hours switches it
// off, which is what a board has until somebody sets one.
//
// The zone is checked here rather than trusted, because a name nobody can load
// would leave the board measuring in UTC while the form claimed otherwise. An
// end before the start is refused for the same reason: SLA.Enabled would read
// it as off, and a promise that silently stopped promising is worse than one
// the form would not accept.
func (k *Kanban) SetBoardSLA(ctx context.Context, id model.ID, sla model.SLA) error {
	if sla.ResponseHours < 0 || sla.ResponseHours > model.MaxResponseHours {
		return invalid("response_hours", fmt.Sprintf("must be between 0 and %d", model.MaxResponseHours))
	}
	if sla.Days&^model.AllDays != 0 {
		return invalid("days", "must be a set of weekdays")
	}
	if sla.Start < 0 || sla.End > model.MinutesPerDay {
		return invalid("hours", "must be inside one day")
	}
	if sla.Start >= sla.End {
		return invalid("hours", "the day must end after it starts")
	}
	if sla.ResponseHours > 0 && !sla.Days.Any() {
		return invalid("days", "pick at least one day the clock runs on")
	}
	if sla.Zone != "" {
		if _, err := time.LoadLocation(sla.Zone); err != nil {
			return invalid("zone", "must be an IANA time zone such as Europe/Zurich")
		}
	}
	b, err := k.store.GetBoardByID(ctx, id)
	if err != nil {
		return err
	}
	b.SLA, b.UpdatedAt = sla, k.now()
	return k.store.UpdateBoard(ctx, b)
}

// DeleteBoard removes a board and everything on it. Nothing brings it back, so
// the confirmation belongs to whatever is calling.
func (k *Kanban) DeleteBoard(ctx context.Context, id model.ID) error {
	return k.store.DeleteBoard(ctx, id)
}

// --- columns ----------------------------------------------------------------

// AddColumn appends a column to a board, after checking its name and that a
// negative WIP limit is not a limit.
func (k *Kanban) AddColumn(ctx context.Context, boardID model.ID, name string, wipLimit int, stopsClock bool) (*model.Column, error) {
	name = strings.TrimSpace(name)
	if err := checkText("name", name, MaxName, true); err != nil {
		return nil, err
	}
	if wipLimit < 0 {
		return nil, invalid("wip_limit", "must not be negative")
	}
	c := &model.Column{ID: k.newID(), BoardID: boardID, Name: name, WIPLimit: wipLimit, StopsClock: stopsClock}
	if err := k.store.CreateColumn(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// UpdateColumn renames a column and sets its limit and whether it stops the
// response clock. Lowering a limit below what the column already holds is
// allowed: the cards are there, and refusing would leave nowhere to put them.
func (k *Kanban) UpdateColumn(ctx context.Context, id model.ID, name string, wipLimit int, stopsClock bool) error {
	name = strings.TrimSpace(name)
	if err := checkText("name", name, MaxName, true); err != nil {
		return err
	}
	if wipLimit < 0 {
		return invalid("wip_limit", "must not be negative")
	}
	return k.store.UpdateColumn(ctx, &model.Column{ID: id, Name: name, WIPLimit: wipLimit, StopsClock: stopsClock})
}

// RemoveColumn deletes a column of boardID, moving its cards to moveCardsTo
// when given and deleting them when it is empty.
//
// The last column cannot go. A board without columns holds no cards and offers
// nowhere to put one, so the delete would leave something that can only be
// repaired through the database.
func (k *Kanban) RemoveColumn(ctx context.Context, boardID, id, moveCardsTo model.ID) error {
	b, err := k.store.GetBoardByID(ctx, boardID)
	if err != nil {
		return err
	}
	if b.Column(id) == nil {
		return store.ErrNotFound
	}
	if len(b.Columns) <= 1 {
		return invalid("column", "a board needs at least one column")
	}
	if moveCardsTo != "" && b.Column(moveCardsTo) == nil {
		return store.ErrNotFound
	}
	return k.store.DeleteColumn(ctx, id, moveCardsTo)
}

// ReorderColumns sets a board's column order. The store checks that the order
// names each column once, because only it can see the whole set.
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

// applyInput copies a form onto a card, checking every field as it goes. Shared
// by create and update so a card cannot be made in a state an edit would refuse.
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

// Card returns one card by id.
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

// SetCardAssignee changes only who a card is assigned to. An empty address
// unassigns it.
//
// Separate from UpdateCard rather than a call through it, because a quick edit
// on the card face knows one field. Routing it through UpdateCard would mean
// sending every other field back to keep it, and two people quick-editing one
// card would then overwrite each other's titles.
func (k *Kanban) SetCardAssignee(ctx context.Context, id model.ID, assignee string) (*model.Card, error) {
	assignee = strings.TrimSpace(assignee)
	if err := checkText("assignee", assignee, MaxAssignee, false); err != nil {
		return nil, err
	}
	c, err := k.store.GetCard(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.Assignee == assignee {
		// Nothing to write, and no reason to move UpdatedAt for a click that
		// changed nothing.
		return c, nil
	}
	c.Assignee, c.UpdatedAt = assignee, k.now()
	if err := k.store.UpdateCard(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// SetCardDueDate changes only the due date. The date is YYYY-MM-DD, as an
// <input type="date"> sends it, and an empty string takes the date off.
//
// Separate from UpdateCard for the same reason as SetCardAssignee: the picker
// on the card face knows one field, and going through UpdateCard would mean
// posting the title and the description back to keep them.
func (k *Kanban) SetCardDueDate(ctx context.Context, id model.ID, due string) (*model.Card, error) {
	date := time.Time{}
	if due = strings.TrimSpace(due); due != "" {
		d, err := time.Parse("2006-01-02", due)
		if err != nil {
			return nil, invalid("due_date", "must be YYYY-MM-DD")
		}
		date = d
	}
	c, err := k.store.GetCard(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.DueDate.Equal(date) {
		// Same as SetCardAssignee: picking the date that is already there is
		// not an edit, so UpdatedAt stays where it is.
		return c, nil
	}
	c.DueDate, c.UpdatedAt = date, k.now()
	if err := k.store.UpdateCard(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// ToggleCardLabel puts a label on a card or takes it off, whichever the card
// is not already. Same reason as SetCardAssignee for not going through
// UpdateCard.
//
// The label has to belong to the card's own board. Nothing else checks that,
// so a crafted id would otherwise attach another board's label to this one.
// ToggleSubtask ticks one line of a card's checklist, or unticks it, and hands
// the card back as it now stands.
//
// By id rather than by position: a card's checklist can be reordered or have a
// line removed in the edit form while somebody else is looking at the board,
// and a position would then tick whatever had moved into that slot.
//
// The write is one field through SetSubtaskDone rather than a read of the card
// and a write of the whole thing. Two people ticking two different lines at the
// same time would both read the card, both flip their own line, and the second
// write would put the first line back; the same is true of a tick landing on
// top of somebody's rename. The read below is only to find out what the line
// currently is and to answer with the card, and a card that changed underneath
// it is a card drawn one tick out of date rather than one silently rolled back.
//
// UpdatedAt moves, like every other write to a card. That restarts the response
// clock, which is right: work on a card is the card being attended to, and a
// desk that looks idle while somebody is working through a checklist is a desk
// whose badges say the wrong thing.
func (k *Kanban) ToggleSubtask(ctx context.Context, id, subtaskID model.ID) (*model.Card, error) {
	c, err := k.store.GetCard(ctx, id)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(c.Subtasks, func(s model.Subtask) bool { return s.ID == subtaskID })
	if i < 0 {
		return nil, store.ErrNotFound
	}
	at := k.now()
	if err := k.store.SetSubtaskDone(ctx, id, subtaskID, !c.Subtasks[i].Done, at); err != nil {
		return nil, err
	}
	c.Subtasks[i].Done = !c.Subtasks[i].Done
	c.UpdatedAt = at
	return c, nil
}

// ToggleCardLabel puts a label on a card or takes it off, refusing a label the
// card's board does not have.
func (k *Kanban) ToggleCardLabel(ctx context.Context, id, labelID model.ID) (*model.Card, error) {
	c, err := k.store.GetCard(ctx, id)
	if err != nil {
		return nil, err
	}
	b, err := k.store.GetBoardByID(ctx, c.BoardID)
	if err != nil {
		return nil, err
	}
	if b.Label(labelID) == nil {
		return nil, store.ErrNotFound
	}
	// No cap on how many: a toggle can only ever add a label the board has, so
	// the board's own label count is the bound.
	if i := slices.Index(c.Labels, labelID); i >= 0 {
		c.Labels = slices.Delete(c.Labels, i, i+1)
	} else {
		c.Labels = append(c.Labels, labelID)
	}
	c.UpdatedAt = k.now()
	if err := k.store.UpdateCard(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// People collects the addresses that already appear on a board, so a quick
// edit can offer the people it knows instead of asking for an address to be
// typed every time. extra is put in front, for the viewer.
//
// It reads the cards the caller already has rather than querying again: the
// only caller is drawing a board it has just loaded. Comment authors are left
// out for the same reason — they would cost a query per board view, and
// someone who has commented is usually someone who has been assigned.
func People(cards []model.Card, extra ...string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(addr string) {
		addr = strings.TrimSpace(addr)
		// Case is not part of an address for our purposes: the same person
		// twice in a menu is a bug, not a feature.
		if addr == "" || seen[strings.ToLower(addr)] {
			return
		}
		seen[strings.ToLower(addr)] = true
		out = append(out, addr)
	}
	for _, e := range extra {
		add(e)
	}
	rest := len(out)
	for _, c := range cards {
		add(c.Assignee)
	}
	// The viewer stays first; everyone else is alphabetical, so the menu does
	// not reshuffle itself as cards move around.
	slices.SortFunc(out[rest:], func(a, b string) int {
		return strings.Compare(strings.ToLower(a), strings.ToLower(b))
	})
	return out
}

// Search returns the cards of a board matching q, in board order, or in
// most-recently-archived order when the query asks for the archive.
//
// An empty query returns the board unfiltered, so the search box can be wired
// to the same handler as the board itself.
func (k *Kanban) Search(ctx context.Context, boardID model.ID, q Query) ([]model.Card, error) {
	board, err := k.store.GetBoardByID(ctx, boardID)
	if err != nil {
		return nil, err
	}

	var cards []model.Card
	if q.Archived {
		cards, err = k.store.ListArchivedCards(ctx, boardID)
	} else {
		cards, err = k.store.ListCards(ctx, boardID)
	}
	if err != nil {
		return nil, err
	}
	if q.Empty() {
		return cards, nil
	}

	// Resolved once for the board rather than per card.
	names := make(map[model.ID]string, len(board.Labels))
	for _, l := range board.Labels {
		names[l.ID] = strings.ToLower(l.Name)
	}
	today := k.now().UTC().Truncate(24 * time.Hour)

	out := make([]model.Card, 0, len(cards))
	for _, c := range cards {
		if q.Match(c, names, today) {
			out = append(out, c)
		}
	}
	return out, nil
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

// DeleteCard removes a card for good. Archiving is the reversible one.
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

// --- comments ---------------------------------------------------------------

// Comments returns a card's thread, oldest first.
func (k *Kanban) Comments(ctx context.Context, cardID model.ID) ([]model.Comment, error) {
	return k.store.ListComments(ctx, cardID)
}

// CommentCounts returns how many comments each card of a board carries, for
// the badge on the card face. Cards without any are absent from the map.
func (k *Kanban) CommentCounts(ctx context.Context, boardID model.ID) (map[model.ID]int, error) {
	return k.store.CountComments(ctx, boardID)
}

// AddComment appends a comment to a card. author is whoever the identity layer
// says is asking; it is stored as given and never looked up.
//
// Commenting moves the card's UpdatedAt, because answering somebody is
// attending to their card and the response-time clock reads UpdatedAt as when
// the card was last attended to. Without this a desk replies to a ticket and
// the badge still turns red, which is how a badge stops being read.
//
// The touch is a second write and it is not in a transaction with the comment.
// A failure there is dropped on purpose: the comment is stored, so the action
// the person took succeeded, and answering them with an error would only get
// the same comment posted twice. What it costs is an SLA badge that stays red
// until the next edit.
func (k *Kanban) AddComment(ctx context.Context, cardID model.ID, author, body string) (*model.Comment, error) {
	body = strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	if err := checkText("body", body, MaxComment, true); err != nil {
		return nil, err
	}
	author = strings.TrimSpace(author)
	if err := checkText("author", author, MaxAssignee, false); err != nil {
		return nil, err
	}
	c := &model.Comment{ID: k.newID(), CardID: cardID, Author: author, Body: body, CreatedAt: k.now()}
	if err := k.store.CreateComment(ctx, c); err != nil {
		return nil, err
	}
	// A comment is a touch on the card: the response-time clock reads
	// UpdatedAt, and an answer in the thread is an answer. One field rather
	// than a read and a whole-row write, so a comment posted while somebody
	// was editing the card cannot put the old title back. The comment is
	// written either way; a card that will not take a timestamp is the store
	// having a bad day, not a reason to lose what somebody typed.
	_ = k.store.TouchCard(ctx, cardID, c.CreatedAt)
	return c, nil
}

// DeleteComment removes a comment, if asker wrote it. It returns the comment
// it removed, so the caller knows which card the thread belonged to.
//
// Two empty addresses count as a match. That is not a hole: it can only happen
// where the deployment has no authentication at all, and there the board has
// exactly one user by definition. Where Cloudflare Access is in front, every
// comment carries an address and this compares two real ones.
func (k *Kanban) DeleteComment(ctx context.Context, id model.ID, asker string) (*model.Comment, error) {
	c, err := k.store.GetComment(ctx, id)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(asker), c.Author) {
		return nil, ErrNotAuthor
	}
	if err := k.store.DeleteComment(ctx, id); err != nil {
		return nil, err
	}
	return c, nil
}

// --- labels -----------------------------------------------------------------

// hexColor is the only shape a label colour may take.
//
// The value goes into a style attribute, where html/template refuses anything
// it cannot prove is a colour and writes ZgotmplZ instead. That renders as a
// broken label rather than as a rejection, so a bad colour is caught here,
// where the user is told what is wrong with it.
var hexColor = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// checkColor accepts a hex colour or nothing at all. Empty means the label
// takes the default grey.
func checkColor(color string) (string, error) {
	color = strings.TrimSpace(color)
	if color == "" {
		return "", nil
	}
	if !hexColor.MatchString(color) {
		return "", invalid("color", "must be a hex colour such as #3b82f6, or empty for the default")
	}
	return strings.ToLower(color), nil
}

// CreateLabel adds a label to a board, checking its name and its colour.
func (k *Kanban) CreateLabel(ctx context.Context, boardID model.ID, name, color string) (*model.Label, error) {
	name = strings.TrimSpace(name)
	if err := checkText("name", name, MaxName, true); err != nil {
		return nil, err
	}
	color, err := checkColor(color)
	if err != nil {
		return nil, err
	}
	l := &model.Label{ID: k.newID(), BoardID: boardID, Name: name, Color: color}
	if err := k.store.CreateLabel(ctx, l); err != nil {
		return nil, err
	}
	return l, nil
}

// UpdateLabel renames a label or recolours it, on every card at once.
func (k *Kanban) UpdateLabel(ctx context.Context, id model.ID, name, color string) error {
	name = strings.TrimSpace(name)
	if err := checkText("name", name, MaxName, true); err != nil {
		return err
	}
	color, err := checkColor(color)
	if err != nil {
		return err
	}
	return k.store.UpdateLabel(ctx, &model.Label{ID: id, Name: name, Color: color})
}

// DeleteLabel removes a label from its board and from every card carrying it.
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
