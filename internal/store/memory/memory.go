// Package memory is an in-memory store.Store. It backs the tests and the
// STORAGE=memory demo mode; nothing survives a restart.
package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"kanban/internal/model"
	"kanban/internal/store"
)

// Store keeps everything in maps behind one mutex.
type Store struct {
	mu       sync.RWMutex
	boards   map[model.ID]*model.Board
	cards    map[model.ID]*model.Card
	comments map[model.ID]*model.Comment
}

// New returns an empty store.
func New() *Store {
	return &Store{
		boards:   map[model.ID]*model.Board{},
		cards:    map[model.ID]*model.Card{},
		comments: map[model.ID]*model.Comment{},
	}
}

var _ store.Store = (*Store)(nil)

// Migrate, Ping and Close have nothing to do here. The schema is a set of maps,
// the backend is this process, and there is nothing to hand back.
func (s *Store) Migrate(context.Context) error { return nil }

// Ping always answers: the backend is this process.
func (s *Store) Ping(context.Context) error { return nil }

// Close has nothing to hand back.
func (s *Store) Close() error { return nil }

// copyBoard returns a board whose slices the caller shares with nobody. Handing
// out the stored value would let somebody's append rewrite what is kept.
func copyBoard(b *model.Board) *model.Board {
	c := *b
	c.Columns = append([]model.Column(nil), b.Columns...)
	c.Labels = append([]model.Label(nil), b.Labels...)
	return &c
}

// copyCard does the same for a card, for the same reason.
func copyCard(k *model.Card) *model.Card {
	c := *k
	c.Labels = append([]model.ID(nil), k.Labels...)
	c.Subtasks = append([]model.Subtask(nil), k.Subtasks...)
	return &c
}

// boardBySlug finds a board by slug. A scan, because the map is keyed by id and
// a store that loses everything on restart never holds many boards.
func (s *Store) boardBySlug(slug string) *model.Board {
	for _, b := range s.boards {
		if b.Slug == slug {
			return b
		}
	}
	return nil
}

// boardOfColumn finds the board a column is on, and the column. Columns live
// inside their board rather than in a map of their own, so one lookup answers
// both questions.
func (s *Store) boardOfColumn(id model.ID) (*model.Board, *model.Column) {
	for _, b := range s.boards {
		if c := b.Column(id); c != nil {
			return b, c
		}
	}
	return nil, nil
}

// dropCard removes a card and the comments on it. The SQL backends get that
// second half from ON DELETE CASCADE; here it has to be written down, and
// written down once, because three different operations delete cards.
func (s *Store) dropCard(id model.ID) {
	delete(s.cards, id)
	for cid, c := range s.comments {
		if c.CardID == id {
			delete(s.comments, cid)
		}
	}
}

// boardOfLabel does the same for a label.
func (s *Store) boardOfLabel(id model.ID) (*model.Board, *model.Label) {
	for _, b := range s.boards {
		if l := b.Label(id); l != nil {
			return b, l
		}
	}
	return nil, nil
}

// sortColumns puts a board's columns in position order. Stable, so two columns
// that somehow share a position keep the order they arrived in.
func sortColumns(b *model.Board) {
	sort.SliceStable(b.Columns, func(i, j int) bool { return b.Columns[i].Position < b.Columns[j].Position })
}

// sortLabels puts a board's labels in name order, which is how every page draws
// them.
func sortLabels(b *model.Board) {
	sort.SliceStable(b.Labels, func(i, j int) bool { return b.Labels[i].Name < b.Labels[j].Name })
}

// ListBoards returns every board, sorted by name, each one copied.
func (s *Store) ListBoards(context.Context) ([]model.Board, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Board, 0, len(s.boards))
	for _, b := range s.boards {
		out = append(out, *copyBoard(b))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Slug < out[j].Slug
	})
	return out, nil
}

// GetBoard returns one board by slug.
func (s *Store) GetBoard(_ context.Context, slug string) (*model.Board, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b := s.boardBySlug(slug)
	if b == nil {
		return nil, store.ErrNotFound
	}
	return copyBoard(b), nil
}

// GetBoardByID returns one board by id, which is what a card knows about.
func (s *Store) GetBoardByID(_ context.Context, id model.ID) (*model.Board, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.boards[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyBoard(b), nil
}

// CreateBoard stores a board and numbers its columns 1..n. A slug already taken
// is a conflict; the store is the only thing that can see the whole set.
func (s *Store) CreateBoard(_ context.Context, b *model.Board) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.boards[b.ID]; ok || s.boardBySlug(b.Slug) != nil {
		return store.ErrConflict
	}
	stored := copyBoard(b)
	// The SQL backends normalise the same way in Go before their insert; here
	// it has to happen too, or a board created without a layout would read back
	// empty and one created with an out-of-range SLA would keep a value the
	// other two would have cleaned.
	stored.Layout = model.LayoutOrDefault(stored.Layout)
	stored.SLA = stored.SLA.Clean()
	for i := range stored.Columns {
		stored.Columns[i].BoardID = b.ID
		stored.Columns[i].Position = i + 1
	}
	sortLabels(stored)
	s.boards[b.ID] = stored
	for i := range b.Columns {
		b.Columns[i].BoardID = b.ID
		b.Columns[i].Position = i + 1
	}
	return nil
}

// UpdateBoard changes a board's own fields and leaves its columns and labels
// alone: those have their own calls, and folding them in here would make every
// rename a chance to drop one.
func (s *Store) UpdateBoard(_ context.Context, b *model.Board) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.boards[b.ID]
	if !ok {
		return store.ErrNotFound
	}
	if o := s.boardBySlug(b.Slug); o != nil && o.ID != b.ID {
		return store.ErrConflict
	}
	cur.Name, cur.Slug, cur.UpdatedAt = b.Name, b.Slug, b.UpdatedAt
	cur.Layout = model.LayoutOrDefault(b.Layout)
	cur.SLA = b.SLA.Clean()
	return nil
}

// DeleteBoard removes a board with its cards and their comments. Nothing else
// points at them, so there is no order to get right.
func (s *Store) DeleteBoard(_ context.Context, id model.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.boards[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.boards, id)
	for cid, c := range s.cards {
		if c.BoardID == id {
			s.dropCard(cid)
		}
	}
	return nil
}

// CreateColumn appends a column to its board and gives it the next position.
func (s *Store) CreateColumn(_ context.Context, c *model.Column) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.boards[c.BoardID]
	if !ok {
		return store.ErrNotFound
	}
	if _, exists := s.boardOfColumn(c.ID); exists != nil {
		return store.ErrConflict
	}
	max := 0
	for _, col := range b.Columns {
		if col.Position > max {
			max = col.Position
		}
	}
	c.Position = max + 1
	b.Columns = append(b.Columns, *c)
	return nil
}

// UpdateColumn changes a column's name, limit and whether it stops the clock.
func (s *Store) UpdateColumn(_ context.Context, c *model.Column) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, cur := s.boardOfColumn(c.ID)
	if cur == nil {
		return store.ErrNotFound
	}
	cur.Name, cur.WIPLimit, cur.StopsClock = c.Name, c.WIPLimit, c.StopsClock
	return nil
}

// DeleteColumn removes a column. Its cards go to moveCardsTo, appended in the
// order they were in, or go with it when no column is named.
func (s *Store) DeleteColumn(_ context.Context, id, moveCardsTo model.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, col := s.boardOfColumn(id)
	if col == nil {
		return store.ErrNotFound
	}
	if moveCardsTo != "" {
		if moveCardsTo == id || b.Column(moveCardsTo) == nil {
			return store.ErrInvalid
		}
		moving := s.columnCards(id)
		max := 0
		for _, c := range s.columnCards(moveCardsTo) {
			if c.Position > max {
				max = c.Position
			}
		}
		for i, c := range moving {
			c.ColumnID = moveCardsTo
			c.Position = max + i + 1
		}
	} else {
		for cid, c := range s.cards {
			if c.ColumnID == id {
				s.dropCard(cid)
			}
		}
	}
	for i := range b.Columns {
		if b.Columns[i].ID == id {
			b.Columns = append(b.Columns[:i], b.Columns[i+1:]...)
			break
		}
	}
	return nil
}

// ReorderColumns sets the order of a board's columns. The order has to name
// every column exactly once, which is checked before anything is written.
func (s *Store) ReorderColumns(_ context.Context, boardID model.ID, order []model.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.boards[boardID]
	if !ok {
		return store.ErrNotFound
	}
	if len(order) != len(b.Columns) {
		return store.ErrInvalid
	}
	seen := map[model.ID]bool{}
	for _, id := range order {
		if seen[id] || b.Column(id) == nil {
			return store.ErrInvalid
		}
		seen[id] = true
	}
	for i, id := range order {
		b.Column(id).Position = i + 1
	}
	sortColumns(b)
	return nil
}

// columnCards returns a column's cards, archived ones included: the callers
// that want them outnumber the ones that do not, and ListCards is the filter ordered by position.
func (s *Store) columnCards(col model.ID) []*model.Card {
	var out []*model.Card
	for _, c := range s.cards {
		if c.ColumnID == col {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ListCards returns a board's live cards in column order, then card order.
func (s *Store) ListCards(_ context.Context, boardID model.ID) ([]model.Card, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.boards[boardID]
	if !ok {
		return nil, nil
	}
	var out []model.Card
	for _, col := range b.Columns {
		for _, c := range s.columnCards(col.ID) {
			if c.Archived() {
				continue
			}
			out = append(out, *copyCard(c))
		}
	}
	return out, nil
}

// ListArchivedCards returns the cards taken off a board, newest first: an
// archive is read from the top.
func (s *Store) ListArchivedCards(_ context.Context, boardID model.ID) ([]model.Card, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.Card
	for _, c := range s.cards {
		if c.BoardID == boardID && c.Archived() {
			out = append(out, *copyCard(c))
		}
	}
	// Most recently archived first, and by id after that so a test that
	// archives several cards in the same instant still sees a stable order.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ArchivedAt.Equal(out[j].ArchivedAt) {
			return out[i].ArchivedAt.After(out[j].ArchivedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// PatchCard writes the fields the patch names and stamps the card, leaving
// everything else as it is, so a quick edit cannot carry back a stale copy of
// the rest of the card.
func (s *Store) PatchCard(_ context.Context, id model.ID, p store.CardPatch, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cards[id]
	if !ok {
		return store.ErrNotFound
	}
	if p.Assignee != nil {
		c.Assignee = *p.Assignee
	}
	if p.DueDate != nil {
		c.DueDate = *p.DueDate
	}
	if p.Labels != nil {
		// Copied, or the caller's slice and the stored one are the same array
		// and a later append rewrites what is kept.
		c.Labels = append([]model.ID(nil), (*p.Labels)...)
	}
	c.UpdatedAt = at.UTC()
	return nil
}

// PatchBoard does the same for a board.
func (s *Store) PatchBoard(_ context.Context, id model.ID, p store.BoardPatch, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.boards[id]
	if !ok {
		return store.ErrNotFound
	}
	if p.Slug != nil && *p.Slug != b.Slug && s.boardBySlug(*p.Slug) != nil {
		return store.ErrConflict
	}
	if p.Name != nil {
		b.Name = *p.Name
	}
	if p.Slug != nil {
		b.Slug = *p.Slug
	}
	if p.Layout != nil {
		b.Layout = *p.Layout
	}
	if p.SLA != nil {
		b.SLA = *p.SLA
	}
	b.UpdatedAt = at.UTC()
	return nil
}

// SetSubtaskDone ticks one checklist line and stamps the card, touching nothing
// else, so two people ticking different lines cannot undo each other.
func (s *Store) SetSubtaskDone(_ context.Context, cardID, subtaskID model.ID, done bool, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cards[cardID]
	if !ok {
		return store.ErrNotFound
	}
	for i := range c.Subtasks {
		if c.Subtasks[i].ID == subtaskID {
			c.Subtasks[i].Done = done
			c.UpdatedAt = at.UTC()
			return nil
		}
	}
	return store.ErrNotFound
}

// TouchCard stamps a card's UpdatedAt and touches nothing else, so a comment
// cannot hand back an edit that landed while it was being written.
func (s *Store) TouchCard(_ context.Context, id model.ID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cards[id]
	if !ok {
		return store.ErrNotFound
	}
	c.UpdatedAt = at.UTC()
	return nil
}

// SetCardArchived takes a card off the board or puts it back. Its column and its
// position are kept, so restoring it returns it where it was.
func (s *Store) SetCardArchived(_ context.Context, id model.ID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cards[id]
	if !ok {
		return store.ErrNotFound
	}
	c.ArchivedAt = at.UTC()
	return nil
}

// GetCard returns one card by id, copied.
func (s *Store) GetCard(_ context.Context, id model.ID) (*model.Card, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.cards[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyCard(c), nil
}

// checkLabels refuses a card carrying a label its board does not have. Without
// it a card could point at a label nothing can draw.
func (s *Store) checkLabels(b *model.Board, labels []model.ID) error {
	for _, id := range labels {
		if b.Label(id) == nil {
			return store.ErrNotFound
		}
	}
	return nil
}

// normalise sorts a card's labels and numbers its subtasks, so two cards with
// the same content compare equal however they were built.
func normalise(c *model.Card) {
	sort.Slice(c.Labels, func(i, j int) bool { return c.Labels[i] < c.Labels[j] })
	for i := range c.Subtasks {
		c.Subtasks[i].Position = i + 1
	}
}

// CreateCard appends a card to its column and gives it the next position.
func (s *Store) CreateCard(_ context.Context, c *model.Card) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.boards[c.BoardID]
	if !ok || b.Column(c.ColumnID) == nil {
		return store.ErrNotFound
	}
	if _, exists := s.cards[c.ID]; exists {
		return store.ErrConflict
	}
	if err := s.checkLabels(b, c.Labels); err != nil {
		return err
	}
	max := 0
	for _, k := range s.columnCards(c.ColumnID) {
		if k.Position > max {
			max = k.Position
		}
	}
	c.Position = max + 1
	stored := copyCard(c)
	// A new card is on the board. The SQL backends get that from leaving
	// archived_at out of the INSERT; here it has to be written, or this would be
	// the only backend where CreateCard can archive one.
	stored.ArchivedAt = time.Time{}
	normalise(stored)
	s.cards[c.ID] = stored
	normalise(c)
	return nil
}

// UpdateCard replaces a card's content and never its column or position: moving
// a card is a different call, and doing both here would move one by accident.
func (s *Store) UpdateCard(_ context.Context, c *model.Card) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.cards[c.ID]
	if !ok {
		return store.ErrNotFound
	}
	if err := s.checkLabels(s.boards[cur.BoardID], c.Labels); err != nil {
		return err
	}
	cur.Title, cur.Description, cur.DueDate, cur.Assignee, cur.UpdatedAt = c.Title, c.Description, c.DueDate, c.Assignee, c.UpdatedAt
	cur.Labels = append([]model.ID(nil), c.Labels...)
	cur.Subtasks = append([]model.Subtask(nil), c.Subtasks...)
	normalise(cur)
	return nil
}

// DeleteCard removes a card and the comments on it.
func (s *Store) DeleteCard(_ context.Context, id model.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cards[id]; !ok {
		return store.ErrNotFound
	}
	s.dropCard(id)
	return nil
}

// ReorderCards sets the order of one column's cards, and moves in any card the
// order names that was somewhere else.
func (s *Store) ReorderCards(_ context.Context, boardID, columnID model.ID, order []model.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.boards[boardID]
	if !ok || b.Column(columnID) == nil {
		return store.ErrNotFound
	}
	listed := map[model.ID]bool{}
	for _, id := range order {
		if listed[id] {
			return store.ErrInvalid
		}
		c, ok := s.cards[id]
		if !ok || c.BoardID != boardID {
			return store.ErrNotFound
		}
		listed[id] = true
	}
	rest := 0
	for _, c := range s.columnCards(columnID) {
		if !listed[c.ID] {
			rest++
			c.Position = len(order) + rest
		}
	}
	for i, id := range order {
		c := s.cards[id]
		c.ColumnID = columnID
		c.Position = i + 1
	}
	return nil
}

// ListComments returns a card's comments oldest first, which is how a thread
// reads.
func (s *Store) ListComments(_ context.Context, cardID model.ID) ([]model.Comment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.Comment
	for _, c := range s.comments {
		if c.CardID == cardID {
			out = append(out, *c)
		}
	}
	// Map iteration is unordered, so the tie-break on ID is what makes two
	// comments written in the same instant come back in the same order twice.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// GetComment returns one comment, for the author check before deleting it.
func (s *Store) GetComment(_ context.Context, id model.ID) (*model.Comment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.comments[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	copied := *c
	return &copied, nil
}

// CreateComment stores a comment against a card that exists.
func (s *Store) CreateComment(_ context.Context, c *model.Comment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cards[c.CardID]; !ok {
		return store.ErrNotFound
	}
	if _, ok := s.comments[c.ID]; ok {
		return store.ErrConflict
	}
	copied := *c
	s.comments[c.ID] = &copied
	return nil
}

// DeleteComment removes one comment. Who may is decided above this.
func (s *Store) DeleteComment(_ context.Context, id model.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.comments[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.comments, id)
	return nil
}

// CountComments returns how many comments each card on a board has, so the
// board can draw the badges without a read per card.
func (s *Store) CountComments(_ context.Context, boardID model.ID) (map[model.ID]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[model.ID]int{}
	for _, c := range s.comments {
		if card, ok := s.cards[c.CardID]; ok && card.BoardID == boardID {
			out[c.CardID]++
		}
	}
	return out, nil
}

// CreateLabel adds a label to its board. A name already used on that board is a
// conflict: two labels with one name cannot be told apart on a card.
func (s *Store) CreateLabel(_ context.Context, l *model.Label) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.boards[l.BoardID]
	if !ok {
		return store.ErrNotFound
	}
	if _, exists := s.boardOfLabel(l.ID); exists != nil {
		return store.ErrConflict
	}
	for _, o := range b.Labels {
		if o.Name == l.Name {
			return store.ErrConflict
		}
	}
	b.Labels = append(b.Labels, *l)
	sortLabels(b)
	return nil
}

// UpdateLabel renames a label or changes its colour, everywhere at once.
func (s *Store) UpdateLabel(_ context.Context, l *model.Label) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, cur := s.boardOfLabel(l.ID)
	if cur == nil {
		return store.ErrNotFound
	}
	for _, o := range b.Labels {
		if o.ID != l.ID && o.Name == l.Name {
			return store.ErrConflict
		}
	}
	cur.Name, cur.Color = l.Name, l.Color
	sortLabels(b)
	return nil
}

// DeleteLabel removes a label from its board and from every card carrying it,
// rather than leaving cards pointing at something that is gone.
func (s *Store) DeleteLabel(_ context.Context, id model.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, cur := s.boardOfLabel(id)
	if cur == nil {
		return store.ErrNotFound
	}
	for i := range b.Labels {
		if b.Labels[i].ID == id {
			b.Labels = append(b.Labels[:i], b.Labels[i+1:]...)
			break
		}
	}
	for _, c := range s.cards {
		if c.BoardID != b.ID {
			continue
		}
		kept := c.Labels[:0]
		for _, lid := range c.Labels {
			if lid != id {
				kept = append(kept, lid)
			}
		}
		c.Labels = kept
	}
	return nil
}
