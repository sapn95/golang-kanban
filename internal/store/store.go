// Package store defines the storage interface every backend implements and
// the small migration runner the SQL backends share.
//
// The interface is coarse-grained on purpose: one method per atomic user
// action, so that backends without transactions (an S3 object per board, a
// Mongo document) can implement it as one read-modify-write. No transaction
// type leaks through it. Business rules live in the service layer.
package store

import (
	"context"
	"errors"
	"time"

	"kanban/internal/model"
)

// Sentinel errors. Backends wrap nothing else into these; callers use
// errors.Is.
var (
	ErrNotFound = errors.New("store: not found")
	ErrConflict = errors.New("store: conflict")         // duplicate slug or label name; ETag mismatch later
	ErrInvalid  = errors.New("store: invalid argument") // e.g. a reorder that does not name every column
)

// Store is implemented by every backend. IDs and timestamps are set by the
// caller; a backend never generates them. Every method is atomic within its
// backend.
type Store interface {
	// Migrate brings the schema up to date. It is safe to call on every start.
	Migrate(ctx context.Context) error
	// Ping reports whether the backend is reachable; /readyz uses it.
	Ping(ctx context.Context) error
	Close() error

	// ListBoards returns every board with its columns and labels, no cards,
	// ordered by name.
	ListBoards(ctx context.Context) ([]model.Board, error)
	// GetBoard returns one board by slug with columns and labels, no cards.
	GetBoard(ctx context.Context, slug string) (*model.Board, error)
	GetBoardByID(ctx context.Context, id model.ID) (*model.Board, error)
	// CreateBoard stores b and its Columns; column positions are assigned
	// 1..n in slice order. ErrConflict on a duplicate slug.
	CreateBoard(ctx context.Context, b *model.Board) error
	// UpdateBoard changes Name, Slug, Layout, SLA and UpdatedAt.
	UpdateBoard(ctx context.Context, b *model.Board) error
	// DeleteBoard removes the board and everything it owns.
	DeleteBoard(ctx context.Context, id model.ID) error

	// CreateColumn appends c to its board; c.Position is set on return.
	CreateColumn(ctx context.Context, c *model.Column) error
	// UpdateColumn changes Name, WIPLimit and StopsClock.
	UpdateColumn(ctx context.Context, c *model.Column) error
	// DeleteColumn removes a column. Its cards are appended to moveCardsTo,
	// a column of the same board, or deleted when moveCardsTo is empty.
	DeleteColumn(ctx context.Context, id, moveCardsTo model.ID) error
	// ReorderColumns sets the order of a board's columns. order must name
	// every column of the board exactly once, else ErrInvalid.
	ReorderColumns(ctx context.Context, boardID model.ID, order []model.ID) error

	// ListCards returns a board's cards with labels and subtasks, ordered by
	// column position, then card position.
	ListCards(ctx context.Context, boardID model.ID) ([]model.Card, error)
	GetCard(ctx context.Context, id model.ID) (*model.Card, error)
	// CreateCard appends c to c.ColumnID, which must belong to c.BoardID;
	// c.Position is set on return.
	CreateCard(ctx context.Context, c *model.Card) error
	// UpdateCard replaces Title, Description, DueDate, Labels, Subtasks and
	// UpdatedAt. It never changes ColumnID or Position.
	UpdateCard(ctx context.Context, c *model.Card) error
	// TouchCard stamps UpdatedAt and leaves the rest of the card alone. A
	// comment is a touch on the card it belongs to, and reading the card back
	// to write one field would hand an edit saved in between straight back to
	// the version the comment started from.
	TouchCard(ctx context.Context, id model.ID, at time.Time) error
	DeleteCard(ctx context.Context, id model.ID) error
	// SetCardArchived takes a card off the board, or puts it back when at is
	// zero. Everything else about the card is left alone, so restoring it
	// returns the same card rather than a copy of it. ListCards never returns
	// an archived card; ListArchivedCards returns only those.
	SetCardArchived(ctx context.Context, id model.ID, at time.Time) error
	// ListArchivedCards returns a board's archived cards, most recently
	// archived first.
	ListArchivedCards(ctx context.Context, boardID model.ID) ([]model.Card, error)
	// ReorderCards is the authoritative order of one column. A listed card
	// that lives in another column of the same board is moved in; cards of
	// the column that are not listed keep their relative order after the
	// listed ones. ErrNotFound for a column or card outside the board,
	// ErrInvalid for a duplicate ID.
	ReorderCards(ctx context.Context, boardID, columnID model.ID, order []model.ID) error

	// ListComments returns a card's comments, oldest first.
	ListComments(ctx context.Context, cardID model.ID) ([]model.Comment, error)
	// GetComment returns one comment. The service reads it to check who wrote
	// it before allowing a delete; the store enforces no such rule itself.
	GetComment(ctx context.Context, id model.ID) (*model.Comment, error)
	// CreateComment appends c to c.CardID. ErrNotFound when the card is gone.
	CreateComment(ctx context.Context, c *model.Comment) error
	// DeleteComment removes one comment. There is no update: comments are
	// append-only, see model.Comment.
	DeleteComment(ctx context.Context, id model.ID) error
	// CountComments returns the number of comments per card of a board, so the
	// board can be drawn with one call rather than one per card. A card with
	// no comments is absent from the map rather than present as zero.
	CountComments(ctx context.Context, boardID model.ID) (map[model.ID]int, error)

	// CreateLabel adds a label to its board. ErrConflict on a duplicate name.
	CreateLabel(ctx context.Context, l *model.Label) error
	UpdateLabel(ctx context.Context, l *model.Label) error
	// DeleteLabel removes the label from every card and deletes it.
	DeleteLabel(ctx context.Context, id model.ID) error
}
