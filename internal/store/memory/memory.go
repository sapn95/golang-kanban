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

func (s *Store) Migrate(context.Context) error { return nil }
func (s *Store) Ping(context.Context) error    { return nil }
func (s *Store) Close() error                  { return nil }

func copyBoard(b *model.Board) *model.Board {
	c := *b
	c.Columns = append([]model.Column(nil), b.Columns...)
	c.Labels = append([]model.Label(nil), b.Labels...)
	return &c
}

func copyCard(k *model.Card) *model.Card {
	c := *k
	c.Labels = append([]model.ID(nil), k.Labels...)
	c.Subtasks = append([]model.Subtask(nil), k.Subtasks...)
	return &c
}

func (s *Store) boardBySlug(slug string) *model.Board {
	for _, b := range s.boards {
		if b.Slug == slug {
			return b
		}
	}
	return nil
}

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

func (s *Store) boardOfLabel(id model.ID) (*model.Board, *model.Label) {
	for _, b := range s.boards {
		if l := b.Label(id); l != nil {
			return b, l
		}
	}
	return nil, nil
}

func sortColumns(b *model.Board) {
	sort.SliceStable(b.Columns, func(i, j int) bool { return b.Columns[i].Position < b.Columns[j].Position })
}

func sortLabels(b *model.Board) {
	sort.SliceStable(b.Labels, func(i, j int) bool { return b.Labels[i].Name < b.Labels[j].Name })
}

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

func (s *Store) GetBoard(_ context.Context, slug string) (*model.Board, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b := s.boardBySlug(slug)
	if b == nil {
		return nil, store.ErrNotFound
	}
	return copyBoard(b), nil
}

func (s *Store) GetBoardByID(_ context.Context, id model.ID) (*model.Board, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.boards[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyBoard(b), nil
}

func (s *Store) CreateBoard(_ context.Context, b *model.Board) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.boards[b.ID]; ok || s.boardBySlug(b.Slug) != nil {
		return store.ErrConflict
	}
	stored := copyBoard(b)
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
	return nil
}

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

func (s *Store) UpdateColumn(_ context.Context, c *model.Column) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, cur := s.boardOfColumn(c.ID)
	if cur == nil {
		return store.ErrNotFound
	}
	cur.Name, cur.WIPLimit = c.Name, c.WIPLimit
	return nil
}

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

// columnCards returns the live cards of a column ordered by position.
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

func (s *Store) GetCard(_ context.Context, id model.ID) (*model.Card, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.cards[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyCard(c), nil
}

func (s *Store) checkLabels(b *model.Board, labels []model.ID) error {
	for _, id := range labels {
		if b.Label(id) == nil {
			return store.ErrNotFound
		}
	}
	return nil
}

func normalise(c *model.Card) {
	sort.Slice(c.Labels, func(i, j int) bool { return c.Labels[i] < c.Labels[j] })
	for i := range c.Subtasks {
		c.Subtasks[i].Position = i + 1
	}
}

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
	normalise(stored)
	s.cards[c.ID] = stored
	normalise(c)
	return nil
}

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

func (s *Store) DeleteCard(_ context.Context, id model.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cards[id]; !ok {
		return store.ErrNotFound
	}
	s.dropCard(id)
	return nil
}

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

func (s *Store) DeleteComment(_ context.Context, id model.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.comments[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.comments, id)
	return nil
}

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
