package api

import (
	"strings"
	"time"

	"kanban/internal/model"
	"kanban/internal/service"
)

// The types in this file are the wire format. They exist instead of putting
// json tags on the model, because the model is the domain and this is a
// published contract: a field renamed in the model would silently rename itself
// in every caller's script, and a field the API should not carry would appear
// there by default. Every one of them is a schema in openapi.json, and a test
// compares the names in both directions.

// dateOnly is how a due date is written, in and out. A due date is a day
// rather than an instant, and a timestamp would invite a timezone question
// nobody asked.
const dateOnly = "2006-01-02"

type indexBody struct {
	Version string `json:"version"`
	Spec    string `json:"spec"`
	Boards  string `json:"boards"`
}

// errorBody is every failure. Field is set when the service refused one named
// field, so a script can say which input was wrong instead of printing a
// sentence.
type errorBody struct {
	Error string `json:"error"`
	Field string `json:"field,omitempty"`
}

type boardBody struct {
	ID        string       `json:"id"`
	Slug      string       `json:"slug"`
	Name      string       `json:"name"`
	Layout    string       `json:"layout"`
	SLA       slaBody      `json:"sla"`
	Columns   []columnBody `json:"columns"`
	Labels    []labelBody  `json:"labels"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

type columnBody struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Position int    `json:"position"`
	// WIPLimit is 0 for a column that holds as many cards as you like.
	WIPLimit int `json:"wip_limit"`
	// StopsClock is a column where the response-time clock does not run.
	StopsClock bool `json:"stops_clock"`
}

// slaBody is the board's response-time promise, in and out.
//
// The days are names and the hours are clock readings, so that a caller reading
// a board in a terminal can see the promise. The model holds a bitmask and
// minutes since midnight; that is storage, not a contract.
type slaBody struct {
	// ResponseHours is the promise in office hours, 0 for a board with none.
	ResponseHours int `json:"response_hours"`
	// Days are the office days, lowercase and three letters: ["mon", "tue"].
	Days []string `json:"days"`
	// Start and End are the office hours as HH:MM. An End of "00:00" is the
	// midnight that ends the day, so a desk staffed around the clock is
	// "00:00" to "00:00".
	Start string `json:"start"`
	End   string `json:"end"`
	// Zone is an IANA name such as Europe/Zurich; empty is UTC.
	Zone string `json:"zone"`
}

type labelBody struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type cardBody struct {
	ID          string `json:"id"`
	BoardID     string `json:"board_id"`
	ColumnID    string `json:"column_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Position    int    `json:"position"`
	// DueDate is YYYY-MM-DD, empty for none.
	DueDate  string `json:"due_date"`
	Assignee string `json:"assignee"`
	// Labels are label ids. The names and colours are on the board.
	Labels   []string      `json:"labels"`
	Subtasks []subtaskBody `json:"subtasks"`
	// ArchivedAt is null while the card is on the board.
	ArchivedAt *time.Time `json:"archived_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type subtaskBody struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Done     bool   `json:"done"`
	Position int    `json:"position"`
}

type commentBody struct {
	ID     string `json:"id"`
	CardID string `json:"card_id"`
	// Author is the address the identity layer supplied, empty where the
	// deployment has no authentication at all.
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// --- what a request sends -----------------------------------------------------

type boardInput struct {
	Name string `json:"name"`
	// Slug is optional; it is derived from the name when it is left out.
	Slug string `json:"slug"`
	// Columns names the columns to create with the board. Left out, the board
	// gets the default three.
	Columns []string `json:"columns"`
}

// boardPatch changes a board's name, its slug, or both. A field left out is
// left alone, which is why this is a PATCH and the two strings are pointers:
// "" is a value here, and the service refuses it.
type boardPatch struct {
	Name *string `json:"name"`
	Slug *string `json:"slug"`
}

type layoutInput struct {
	Layout string `json:"layout"`
}

type columnInput struct {
	Name     string `json:"name"`
	WIPLimit int    `json:"wip_limit"`
	// StopsClock takes the column out of the response-time promise.
	StopsClock bool `json:"stops_clock"`
}

// slaInput is a whole promise, the same shape it is read in. A PUT with no
// response_hours switches the SLA off, the way the settings form does when the
// hours are cleared.
type slaInput slaBody

// orderInput is the whole order of one column, or of a board's columns.
type orderInput struct {
	Order []string `json:"order"`
}

type labelInput struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// cardInput is every field of a card that a request may set. It is the whole
// card on both POST and PUT: a PUT with no description clears the description,
// the same way the edit form does when the field is emptied.
type cardInput struct {
	// ColumnID is where the card goes. Required on create; on update it moves
	// the card, and left out the card stays where it is.
	ColumnID    string         `json:"column_id"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	DueDate     string         `json:"due_date"`
	Assignee    string         `json:"assignee"`
	Labels      []string       `json:"labels"`
	Subtasks    []subtaskInput `json:"subtasks"`
}

// subtaskInput carries an id for a subtask that already exists, so ticking one
// off does not replace it with a new row. Leave it out for a new subtask. The
// order of the list is the order on the card.
type subtaskInput struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

type assigneeInput struct {
	// Assignee is an address, "@me" for whoever is making the request, or
	// empty to take the card off everybody's list.
	Assignee string `json:"assignee"`
}

type commentInput struct {
	Body string `json:"body"`
}

type bulkInput struct {
	// Action is move, assign, archive or delete.
	Action string   `json:"action"`
	IDs    []string `json:"ids"`
	// Target is the destination column for move, and the address or "@me" for
	// assign. It is ignored by the other two.
	Target string `json:"target"`
}

// bulkBody reports what a bulk request did. The operations are independent, so
// a card somebody else deleted a second ago is one entry in failed rather than
// a failure of the request.
type bulkBody struct {
	Changed []string `json:"changed"`
	// Failed maps a card id to why that card did not change.
	Failed map[string]string `json:"failed"`
}

// --- model to wire ------------------------------------------------------------

func toBoard(b model.Board) boardBody {
	return boardBody{
		ID:        string(b.ID),
		Slug:      b.Slug,
		Name:      b.Name,
		Layout:    model.LayoutOrDefault(b.Layout),
		SLA:       toSLA(b.SLA),
		Columns:   toColumns(b.Columns),
		Labels:    toLabels(b.Labels),
		CreatedAt: b.CreatedAt,
		UpdatedAt: b.UpdatedAt,
	}
}

func toBoards(boards []model.Board) []boardBody {
	out := make([]boardBody, 0, len(boards))
	for _, b := range boards {
		out = append(out, toBoard(b))
	}
	return out
}

func toColumn(c model.Column) columnBody {
	return columnBody{
		ID: string(c.ID), Name: c.Name, Position: c.Position,
		WIPLimit: c.WIPLimit, StopsClock: c.StopsClock,
	}
}

// toSLA writes the promise out. Days is never null: a caller looping over it
// should not have to check, and a board with no office days is a promise that is
// off rather than a missing field.
func toSLA(s model.SLA) slaBody {
	days := s.Days.Names()
	if days == nil {
		days = []string{}
	}
	return slaBody{
		ResponseHours: s.ResponseHours,
		Days:          days,
		Start:         model.ClockString(s.Start),
		End:           model.ClockString(s.End),
		Zone:          s.Zone,
	}
}

func toColumns(columns []model.Column) []columnBody {
	out := make([]columnBody, 0, len(columns))
	for _, c := range columns {
		out = append(out, toColumn(c))
	}
	return out
}

func toLabel(l model.Label) labelBody {
	return labelBody{ID: string(l.ID), Name: l.Name, Color: l.Color}
}

func toLabels(labels []model.Label) []labelBody {
	out := make([]labelBody, 0, len(labels))
	for _, l := range labels {
		out = append(out, toLabel(l))
	}
	return out
}

func toCard(c model.Card) cardBody {
	body := cardBody{
		ID:          string(c.ID),
		BoardID:     string(c.BoardID),
		ColumnID:    string(c.ColumnID),
		Title:       c.Title,
		Description: c.Description,
		Position:    c.Position,
		Assignee:    c.Assignee,
		Labels:      make([]string, 0, len(c.Labels)),
		Subtasks:    make([]subtaskBody, 0, len(c.Subtasks)),
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
	if !c.DueDate.IsZero() {
		body.DueDate = c.DueDate.Format(dateOnly)
	}
	if c.Archived() {
		at := c.ArchivedAt
		body.ArchivedAt = &at
	}
	for _, id := range c.Labels {
		body.Labels = append(body.Labels, string(id))
	}
	for _, st := range c.Subtasks {
		body.Subtasks = append(body.Subtasks, subtaskBody{
			ID: string(st.ID), Title: st.Title, Done: st.Done, Position: st.Position,
		})
	}
	return body
}

func toCards(cards []model.Card) []cardBody {
	out := make([]cardBody, 0, len(cards))
	for _, c := range cards {
		out = append(out, toCard(c))
	}
	return out
}

func toComment(c model.Comment) commentBody {
	return commentBody{
		ID:        string(c.ID),
		CardID:    string(c.CardID),
		Author:    c.Author,
		Body:      c.Body,
		CreatedAt: c.CreatedAt,
	}
}

func toComments(comments []model.Comment) []commentBody {
	out := make([]commentBody, 0, len(comments))
	for _, c := range comments {
		out = append(out, toComment(c))
	}
	return out
}

// --- wire to service ----------------------------------------------------------

func (in cardInput) toService() service.CardInput {
	labels := make([]model.ID, 0, len(in.Labels))
	for _, id := range in.Labels {
		labels = append(labels, model.ID(id))
	}
	subtasks := make([]model.Subtask, 0, len(in.Subtasks))
	for _, st := range in.Subtasks {
		subtasks = append(subtasks, model.Subtask{ID: model.ID(st.ID), Title: st.Title, Done: st.Done})
	}
	return service.CardInput{
		Title:       in.Title,
		Description: in.Description,
		DueDate:     in.DueDate,
		Assignee:    in.Assignee,
		Labels:      labels,
		Subtasks:    subtasks,
	}
}

// toModel reads a promise in. The names and the clock readings are parsed here
// rather than in the service, because they are this package's format: the
// settings form posts the same words and goes through the same two parsers.
//
// An empty start or end is the default office day, so a caller who only wants to
// set the hours of the promise does not have to restate 08:00 to 17:00.
func (in slaInput) toModel() (model.SLA, error) {
	sla := model.DefaultSLA()
	sla.ResponseHours = in.ResponseHours
	sla.Zone = strings.TrimSpace(in.Zone)
	days, err := model.ParseDays(in.Days)
	if err != nil {
		return sla, &service.ValidationError{Field: "days", Message: err.Error()}
	}
	if in.Days != nil {
		sla.Days = days
	}
	if v := strings.TrimSpace(in.Start); v != "" {
		if sla.Start, err = model.ParseClock(v, false); err != nil {
			return sla, &service.ValidationError{Field: "start", Message: err.Error()}
		}
	}
	if v := strings.TrimSpace(in.End); v != "" {
		if sla.End, err = model.ParseClock(v, true); err != nil {
			return sla, &service.ValidationError{Field: "end", Message: err.Error()}
		}
	}
	return sla, nil
}

func toIDs(ids []string) []model.ID {
	out := make([]model.ID, 0, len(ids))
	for _, id := range ids {
		out = append(out, model.ID(id))
	}
	return out
}
