package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"kanban/internal/model"
	"kanban/internal/store"
)

// Options configures Import.
type Options struct {
	// Replace deletes a board that is already in the store before writing the
	// one from the snapshot. Without it an existing board is an error, so that
	// importing into the wrong database says so instead of doubling everything.
	//
	// A board is matched by ID and by slug, because a restore has to work both
	// when the board is still there and when it was recreated by hand under the
	// same name while the snapshot was being fetched.
	Replace bool
}

// Report says what an import did, or what a check says it would do.
type Report struct {
	Boards   int
	Replaced int
	Columns  int
	Labels   int
	Cards    int
	Comments int
}

// String is the line the command prints and the log records.
func (r Report) String() string {
	out := strings.Join([]string{
		plural(r.Boards, "board"),
		plural(r.Columns, "column"),
		plural(r.Labels, "label"),
		plural(r.Cards, "card"),
		plural(r.Comments, "comment"),
	}, ", ")
	if r.Replaced > 0 {
		out += fmt.Sprintf(" (%s overwritten)", plural(r.Replaced, "existing board"))
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// Import writes a snapshot into a store.
//
// The whole document is checked before the first write, so the failure that
// actually happens in practice, a file that is truncated or edited wrong, costs
// nothing. What cannot be promised is atomicity across boards: the store
// interface has no transaction ([0003]) precisely so that backends without one
// can implement it, and a restore that needed one would leak that requirement
// into every backend. An import that fails half way leaves the boards it has
// already written, and running it again with Replace finishes the job.
//
// The snapshot is trusted input. It is a file an operator hands to a command,
// not a request: nothing in the HTTP surface imports one.
func Import(ctx context.Context, s store.Store, snap *Snapshot, opts Options) (Report, error) {
	var rep Report
	if _, err := Check(snap); err != nil {
		return rep, err
	}
	if !opts.Replace {
		if err := checkAbsent(ctx, s, snap); err != nil {
			return rep, err
		}
	}
	for _, b := range snap.Boards {
		if err := importBoard(ctx, s, b, opts, &rep); err != nil {
			return rep, fmt.Errorf("import board %s: %w", b.Slug, err)
		}
	}
	return rep, nil
}

// Check validates a snapshot and reports what importing it would write,
// without touching the store. It is what `kanban import -dry-run` runs, so that
// a file can be looked over before a restore rather than after one.
func Check(snap *Snapshot) (Report, error) {
	var rep Report
	if snap == nil {
		return rep, fmt.Errorf("%w: no snapshot", ErrInvalid)
	}
	if snap.Format == 0 || snap.Format > Format {
		return rep, fmt.Errorf("%w: format %d, this build writes %d", ErrFormat, snap.Format, Format)
	}
	if err := validate(snap); err != nil {
		return rep, err
	}
	for _, b := range snap.Boards {
		rep.Boards++
		rep.Columns += len(b.Columns)
		rep.Labels += len(b.Labels)
		rep.Cards += len(b.Cards)
		for _, c := range b.Cards {
			rep.Comments += len(c.Comments)
		}
	}
	return rep, nil
}

// validate rejects a snapshot that could only be written half way.
//
// Every ID has to be unique across the whole file, not only within its board:
// IDs are 130 random bits ([0003]), so two the same are a file someone has
// copied a block in, and the store would answer with a conflict several
// hundred writes later.
func validate(snap *Snapshot) error {
	ids := map[string]string{} // id -> what claimed it
	claim := func(kind, id string) error {
		if id == "" {
			return fmt.Errorf("%w: a %s has no id", ErrInvalid, kind)
		}
		if was, dup := ids[id]; dup {
			return fmt.Errorf("%w: id %s is used by two things (%s and %s)", ErrInvalid, id, was, kind)
		}
		ids[id] = kind
		return nil
	}
	slugs := map[string]bool{}
	for _, b := range snap.Boards {
		if err := claim("board", b.ID); err != nil {
			return err
		}
		if b.Slug == "" {
			return fmt.Errorf("%w: board %s has no slug", ErrInvalid, b.ID)
		}
		if slugs[b.Slug] {
			return fmt.Errorf("%w: two boards have the slug %s", ErrInvalid, b.Slug)
		}
		slugs[b.Slug] = true
		if len(b.Columns) == 0 {
			// The service refuses to delete the last column for the same
			// reason: a board without one holds no cards and offers nowhere to
			// put one, and only the database could repair it.
			return fmt.Errorf("%w: board %s has no columns", ErrInvalid, b.Slug)
		}
		columns := map[string]bool{}
		for _, c := range b.Columns {
			if err := claim("column", c.ID); err != nil {
				return err
			}
			columns[c.ID] = true
		}
		labels := map[string]bool{}
		for _, l := range b.Labels {
			if err := claim("label", l.ID); err != nil {
				return err
			}
			if l.Name == "" {
				return fmt.Errorf("%w: label %s of board %s has no name", ErrInvalid, l.ID, b.Slug)
			}
			labels[l.ID] = true
		}
		for _, c := range b.Cards {
			if err := claim("card", c.ID); err != nil {
				return err
			}
			if !columns[c.ColumnID] {
				return fmt.Errorf("%w: card %s names column %s, which board %s does not have",
					ErrInvalid, c.ID, c.ColumnID, b.Slug)
			}
			for _, id := range c.Labels {
				if !labels[id] {
					return fmt.Errorf("%w: card %s names label %s, which board %s does not have",
						ErrInvalid, c.ID, id, b.Slug)
				}
			}
			if c.DueDate != "" {
				if _, err := time.Parse(dateOnly, c.DueDate); err != nil {
					return fmt.Errorf("%w: card %s has due_date %q, want YYYY-MM-DD", ErrInvalid, c.ID, c.DueDate)
				}
			}
			for _, st := range c.Subtasks {
				if err := claim("subtask", st.ID); err != nil {
					return err
				}
			}
			for _, m := range c.Comments {
				if err := claim("comment", m.ID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// checkAbsent reports the first board of the snapshot that the store already
// holds, before anything has been written.
func checkAbsent(ctx context.Context, s store.Store, snap *Snapshot) error {
	for _, b := range snap.Boards {
		if _, err := s.GetBoardByID(ctx, model.ID(b.ID)); err == nil {
			return fmt.Errorf("%w: id %s; import with -replace to overwrite it", ErrExists, b.ID)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if _, err := s.GetBoard(ctx, b.Slug); err == nil {
			return fmt.Errorf("%w: slug %s; import with -replace to overwrite it", ErrExists, b.Slug)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

func importBoard(ctx context.Context, s store.Store, b Board, opts Options, rep *Report) error {
	if opts.Replace {
		n, err := deleteExisting(ctx, s, b)
		if err != nil {
			return err
		}
		rep.Replaced += n
	}

	board := &model.Board{
		ID:        model.ID(b.ID),
		Slug:      b.Slug,
		Name:      b.Name,
		Layout:    model.LayoutOrDefault(b.Layout),
		CreatedAt: b.CreatedAt,
		UpdatedAt: b.UpdatedAt,
	}
	for _, c := range b.Columns {
		board.Columns = append(board.Columns, model.Column{
			ID:       model.ID(c.ID),
			BoardID:  board.ID,
			Name:     c.Name,
			WIPLimit: c.WIPLimit,
		})
	}
	if err := s.CreateBoard(ctx, board); err != nil {
		return fmt.Errorf("create board: %w", err)
	}
	rep.Boards++
	rep.Columns += len(board.Columns)

	// Labels first: a card names them, and CreateCard checks that the board has
	// the ones it names.
	for _, l := range b.Labels {
		label := &model.Label{ID: model.ID(l.ID), BoardID: board.ID, Name: l.Name, Color: l.Color}
		if err := s.CreateLabel(ctx, label); err != nil {
			return fmt.Errorf("create label %s: %w", l.Name, err)
		}
		rep.Labels++
	}

	for _, c := range b.Cards {
		if err := importCard(ctx, s, board.ID, c, rep); err != nil {
			return fmt.Errorf("create card %s: %w", c.ID, err)
		}
	}
	return nil
}

// deleteExisting removes whatever already holds this board's id or its slug,
// and returns how many boards that was. DeleteBoard takes the cards, comments
// and labels with it.
func deleteExisting(ctx context.Context, s store.Store, b Board) (int, error) {
	victims := map[model.ID]bool{}
	if cur, err := s.GetBoardByID(ctx, model.ID(b.ID)); err == nil {
		victims[cur.ID] = true
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	if cur, err := s.GetBoard(ctx, b.Slug); err == nil {
		victims[cur.ID] = true
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	for id := range victims {
		if err := s.DeleteBoard(ctx, id); err != nil {
			return 0, fmt.Errorf("delete board %s: %w", id, err)
		}
	}
	return len(victims), nil
}

// importCard writes one card, then its comments.
//
// The card is created on the board even when the snapshot says it is archived,
// and archived afterwards. CreateCard is documented to append a card to its
// column and says nothing about ArchivedAt, so a backend is within its rights
// to ignore the field; SetCardArchived is the one way to archive that every
// backend has to implement.
//
// A card's position is assigned by the store, so the order of Board.Cards is
// what carries over and the numbers do not. The consequence worth knowing is
// that a restored archive comes back at the end of its column rather than in
// the row it left.
func importCard(ctx context.Context, s store.Store, boardID model.ID, c Card, rep *Report) error {
	card := &model.Card{
		ID:          model.ID(c.ID),
		BoardID:     boardID,
		ColumnID:    model.ID(c.ColumnID),
		Title:       c.Title,
		Description: c.Description,
		Assignee:    c.Assignee,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
	if c.DueDate != "" {
		due, err := time.Parse(dateOnly, c.DueDate)
		if err != nil {
			return fmt.Errorf("due_date %q: %w", c.DueDate, err)
		}
		card.DueDate = due
	}
	for _, id := range c.Labels {
		card.Labels = append(card.Labels, model.ID(id))
	}
	for i, st := range c.Subtasks {
		card.Subtasks = append(card.Subtasks, model.Subtask{
			ID:       model.ID(st.ID),
			Title:    st.Title,
			Done:     st.Done,
			Position: i + 1,
		})
	}
	if err := s.CreateCard(ctx, card); err != nil {
		return err
	}
	rep.Cards++
	if c.ArchivedAt != nil {
		if err := s.SetCardArchived(ctx, card.ID, *c.ArchivedAt); err != nil {
			return fmt.Errorf("archive: %w", err)
		}
	}
	for _, m := range c.Comments {
		comment := &model.Comment{
			ID:        model.ID(m.ID),
			CardID:    card.ID,
			Author:    m.Author,
			Body:      m.Body,
			CreatedAt: m.CreatedAt,
		}
		if err := s.CreateComment(ctx, comment); err != nil {
			return fmt.Errorf("create comment %s: %w", m.ID, err)
		}
		rep.Comments++
	}
	return nil
}
