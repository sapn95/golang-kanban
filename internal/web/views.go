// The shapes a template is handed. Everything here turns a model into
// something a page can print, and nothing here touches a request.

package web

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"kanban/internal/identity"
	"kanban/internal/model"
	"kanban/internal/service"
)

// readableOn returns a text colour that can be read on the given background.
//
// Labels used to print white on whatever colour they carried, which is fine on
// the red and the blue and close to invisible on the yellow. The palette holds
// both light and dark entries, so the choice cannot be made once for all of
// them. An unparseable colour gets white, which is what it had before.
func readableOn(background string) string {
	const dark, light = "#111827", "#ffffff"
	r, g, b, ok := parseHex(background)
	if !ok {
		return light
	}
	// Rec. 601 luma. The threshold is where white and near-black are about
	// equally readable; it does not need to be more exact than that.
	if 299*r+587*g+114*b > 150_000 {
		return dark
	}
	return light
}

// parseHex reads a #rgb or #rrggbb colour. The second return says whether it
// read one at all, so a colour that is not one gets a readable default.
func parseHex(s string) (r, g, b int, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) == 3 {
		// #abc is #aabbcc; doubling the digit is the definition, not a guess.
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return int(v>>16) & 0xff, int(v>>8) & 0xff, int(v) & 0xff, true
}

// --- view models --------------------------------------------------------------

type cardView struct {
	// Viewer is who is looking, so a card can offer "assign to me" and mark
	// the ones that are already theirs.
	Viewer identity.User
	Card   model.Card
	// BoardSlug is in the card so a label chip can link to a search of its own
	// board. The card face is rendered from several handlers and not only from
	// the board page, so it cannot reach up to the board being drawn around it.
	BoardSlug   string
	Labels      []model.Label  // resolved from the board
	BoardLabels []model.Label  // every label of the board, for the edit form
	Columns     []model.Column // every column, so the edit form can move the card
	Due         string         // "" or "2 Jan 2006"
	DueInput    string         // "" or "2006-01-02"
	// DueState is "", "overdue", "today", "soon" or "later". A single flag
	// only distinguished late from not late, which said nothing about a card
	// due in an hour and a card due in a month.
	DueState string
	// SLAState is "", "ok", "soon" or "breached": how long the card has sat
	// without anybody touching it, against the board's response-time promise.
	// Empty is nobody waiting, which the card face draws as no badge at all.
	//
	// A due date is a promise to whoever the card is for; this is a promise
	// about answering, so a card can be a week from its date and already late.
	SLAState string
	// SLALeft is "3h left" or "2h over", counted in office hours. SLADue is the
	// deadline itself in the board's own zone, for the title attribute.
	SLALeft string
	SLADue  string
	// SubtasksDone and SubtasksTotal draw the checklist's progress on the card
	// face. Percent is separate because a template cannot divide.
	SubtasksDone    int
	SubtasksTotal   int
	SubtasksPercent int
	// CommentCount draws the badge on the card face. The board loads every
	// card's count in one call; a single-card fragment counts its own.
	CommentCount int
	// Comments is the thread, filled only for the edit form. The board does
	// not read it, because that would be one query per card on every render.
	Comments []commentView
	// OOB marks the card as an out-of-band swap: the answer to a comment
	// carries the card face along so its badge is not left stale behind the
	// open modal. htmx ignores it when the board is not the page on screen.
	OOB bool
	// People are the addresses the quick-edit menu offers, viewer first.
	People []string
}

type commentView struct {
	Comment model.Comment
	// Mine is whether the viewer wrote it, which is who may remove it.
	Mine bool
	// Ago is "3 hours ago"; Exact is the instant, for the title attribute.
	Ago   string
	Exact string
}

type columnView struct {
	Column model.Column
	Cards  []cardView
	Count  int
	// AtLimit is a full column, OverLimit one that is already past. They are
	// separate because the first is working as intended and the second is not,
	// and a column that only turns red once the rule has been broken tells
	// nobody it was about to be.
	AtLimit   bool
	OverLimit bool
	// LimitPercent fills the bar under the header, capped at 100 so an
	// over-full column does not draw outside its own track.
	LimitPercent int
	// OOB marks the header as one htmx should take out of the response and
	// put back where it belongs, rather than swap into the target. Set on
	// every response that changed a count and left the page standing.
	OOB bool
}

// columnHead is one column header's view: the count, and where that count
// stands against the limit.
//
// The arithmetic lives here rather than in the browser because the header is
// the WIP limit's whole interface, and a copy of these rules in JavaScript was
// a second place for "what does a full column look like" to be answered.
func columnHead(col model.Column, count int, oob bool) columnView {
	cv := columnView{Column: col, Count: count, OOB: oob}
	if col.WIPLimit > 0 {
		cv.AtLimit = count == col.WIPLimit
		cv.OverLimit = count > col.WIPLimit
		cv.LimitPercent = min(count*100/col.WIPLimit, 100)
	}
	return cv
}

type boardPage struct {
	Title string
	User  identity.User
	// Boards is every board, for the switcher the app bar puts behind this
	// one's name.
	Boards []model.Board
	// Query is what the user typed, echoed back into the box so a reload or a
	// shared link keeps the search.
	Query   string
	Results []cardView
	// Searching is Query being non-empty, not Results being non-empty: a
	// search with no hits must say so rather than silently showing the board.
	Searching bool
	BoardSlug string
	Board     *model.Board
	Columns   []columnView
	EmptyCard cardView
	// Rows is the board drawn as stacked rows rather than side-by-side
	// columns. Resolved here so the template asks a boolean rather than
	// comparing strings.
	Rows bool
}

type archivePage struct {
	Title     string
	User      identity.User
	BoardSlug string
	// Boards is every board, for the switcher the app bar puts behind this
	// one's name.
	Boards []model.Board
	Board  *model.Board
	Cards  []cardView
	// Query is the raw search, echoed back into the box so reloading keeps it.
	// Searching says the box had something in it, which is the difference
	// between an empty archive and a search that found nothing in it.
	Query     string
	Searching bool
}

type boardsPage struct {
	Title     string
	User      identity.User
	BoardSlug string
	Boards    []model.Board
	Error     string
}

// labelPalette is the set of colours the label form offers on every board.
//
// A board reads better when its labels come from one set of colours than when
// every one is picked by hand, and a fixed list cannot produce a value the
// template has to refuse. The service still accepts any hex colour, so an API
// client is not held to these ten.
//
// Roughly a spectrum, warm to cool, so neighbouring swatches are the ones that
// look alike and the list can be scanned rather than read.
var labelPalette = []string{
	"#ef4444", "#f97316", "#f59e0b", "#eab308", "#22c55e",
	"#0ea5e9", "#3b82f6", "#8b5cf6", "#ec4899", "#6b7280",
}

// boardPalette is what one board's label rows offer: the presets, plus any
// colour a label on this board already carries that is not one of them.
//
// The colours in use have to be in every row, not only in the row that carries
// one. A colour set through the API was offered to the label that had it and to
// nothing else, so a board with a label in a colour off this list could not be
// given a second label to match it, and each row showed a different number of
// swatches for a reason nobody could see from the page.
func boardPalette(labels []model.Label) []string {
	palette := slices.Clone(labelPalette)
	var extra []string
	for _, l := range labels {
		if l.Color != "" && !slices.Contains(palette, l.Color) && !slices.Contains(extra, l.Color) {
			extra = append(extra, l.Color)
		}
	}
	// Sorted, so the row does not reshuffle when a label is renamed: the labels
	// arrive in name order, and appending in that order would move a swatch.
	slices.Sort(extra)
	return append(palette, extra...)
}

type labelView struct {
	Label model.Label
	// Cards is how many cards carry the label, archived ones included.
	// Deleting takes it off all of them, so the page says so first.
	Cards int
	// Palette is boardPalette, the same list in every row, so a colour one
	// label carries can be given to another.
	Palette []string
}

type columnSetting struct {
	Column model.Column
	// Cards is how many are in it, the archive included, because deleting the
	// column decides what happens to those as well.
	Cards int
	// Others are where its cards could go instead, on a delete.
	Others []model.Column
	First  bool
	Last   bool
	// Only marks the last column standing. It cannot be deleted, so the form
	// does not offer to.
	Only bool
}

// slaDay is one weekday checkbox in the response-time form.
type slaDay struct {
	// Value is what the checkbox posts: the day's name, the same three letters
	// the JSON API takes, so one parser reads both and a day cannot mean
	// Tuesday in a form and Wednesday in a script.
	Value   string // "mon"
	Short   string // "Mon"
	Checked bool
}

type slaView struct {
	// On is the promise being in force, which is what decides whether the page
	// shows a summary line or says the clock is off.
	On    bool
	Hours int
	Days  []slaDay
	// Start and End are "08:00", as <input type="time"> wants them. An end of
	// "00:00" is midnight at the end of the day, so a desk staffed around the
	// clock is 00:00 to 00:00; a time input cannot say 24:00.
	Start string
	End   string
	Zone  string
	// Zones is what the picker offers, by region, with the board's own zone
	// kept on the list even when it is not one of the generated names.
	Zones []model.ZoneGroup
	// Summary is the promise in one line, under the form.
	Summary string
}

// slaSettings builds the response-time form.
//
// A board whose promise is off may hold hours nobody chose, from a migration
// default or from an API client that sent none. The form falls back to the
// office week for those, so it always opens on something somebody would want
// rather than on Monday 00:00 to 00:00.
func slaSettings(sla model.SLA) slaView {
	d := model.DefaultSLA()
	if sla.Start >= sla.End {
		sla.Start, sla.End = d.Start, d.End
	}
	if !sla.Days.Any() {
		sla.Days = d.Days
	}
	v := slaView{
		On:    sla.Enabled(),
		Hours: sla.ResponseHours,
		Start: clockValue(sla.Start),
		End:   clockValue(sla.End),
		Zone:  sla.Zone,
		Zones: model.Zones(sla.Zone),
	}
	// A board with no promise still shows a number, so that turning the switch
	// on and saving asks for nothing else. Zero in the box would read as "off,
	// and also zero hours", which is the confusion the switch is there to end.
	// Eight because it is the office day the rest of the form opens on.
	if v.Hours <= 0 {
		v.Hours = 8
	}
	for _, w := range []time.Weekday{time.Monday, time.Tuesday, time.Wednesday,
		time.Thursday, time.Friday, time.Saturday, time.Sunday} {
		v.Days = append(v.Days, slaDay{
			Value: strings.ToLower(w.String()[:3]), Short: w.String()[:3], Checked: sla.Days.Has(w),
		})
	}
	if v.On {
		zone := sla.Zone
		if zone == "" {
			zone = "UTC"
		}
		v.Summary = fmt.Sprintf("%s, %s, %s to %s %s",
			plural(sla.ResponseHours, "office hour"), sla.Days, v.Start, v.End, zone)
	}
	return v
}

// clockValue renders minutes since midnight for an <input type="time">. The end
// of the day is 1440 minutes in, which the input cannot hold, so it goes back as
// the midnight it is.
func clockValue(m int) string { return model.ClockString(m) }

type settingsPage struct {
	Title     string
	User      identity.User
	BoardSlug string
	// Boards is every board, for the switcher the app bar puts behind this
	// one's name.
	Boards   []model.Board
	Board    *model.Board
	Columns  []columnSetting
	Labels   []labelView
	NewLabel labelView
	SLA      slaView
	Error    string
}

// cardView builds one card face. clock is the board's SLA clock, passed in
// rather than resolved here: resolving one reads the zone database, and a board
// page grades every card it draws against the same promise.
func (s *Server) cardView(u identity.User, b *model.Board, clock model.Clock, c model.Card, comments int, people []string) cardView {
	v := cardView{Viewer: u, Card: c, BoardSlug: b.Slug, BoardLabels: b.Labels, Columns: b.Columns, CommentCount: comments, People: people}
	now := s.now().UTC()
	if state, left := clock.CardState(c, b.Column(c.ColumnID), now); state != model.SLAOff {
		v.SLAState = state
		v.SLALeft = slaLeft(left)
		v.SLADue = clock.Deadline(c.UpdatedAt).In(clock.Location()).Format("2 Jan 2006, 15:04 MST")
	}
	for _, id := range c.Labels {
		if l := b.Label(id); l != nil {
			v.Labels = append(v.Labels, *l)
		}
	}
	if !c.DueDate.IsZero() {
		v.Due = c.DueDate.Format("2 Jan 2006")
		v.DueInput = c.DueDate.Format("2006-01-02")
		v.DueState = dueState(s.now().UTC().Truncate(24*time.Hour), c.DueDate)
	}
	for _, st := range c.Subtasks {
		if st.Done {
			v.SubtasksDone++
		}
	}
	v.SubtasksTotal = len(c.Subtasks)
	if v.SubtasksTotal > 0 {
		v.SubtasksPercent = v.SubtasksDone * 100 / v.SubtasksTotal
	}
	return v
}

// slaLeft renders the office time either side of the deadline: "3h 20m left"
// while there is time, "2h over" once there is not.
//
// Office time, which is why it does not say "in 3 hours": three office hours on
// a Friday afternoon are Monday lunchtime, and a badge that said "in 3 hours"
// there would be read as wrong rather than as precise.
func slaLeft(d time.Duration) string {
	over := d <= 0
	if over {
		d = -d
	}
	d = d.Round(time.Minute)
	var s string
	switch h, m := int(d.Hours()), int(d.Minutes())%60; {
	case h > 0 && m > 0:
		s = fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		s = fmt.Sprintf("%dh", h)
	default:
		s = fmt.Sprintf("%dm", m)
	}
	if over {
		return s + " over"
	}
	return s + " left"
}

// dueSoon is how far ahead a due date still counts as pressing. Three days is
// long enough to act on and short enough that most of the board is not amber.
const dueSoon = 3

// dueState grades a due date against today. Both are dates at midnight UTC.
func dueState(today, due time.Time) string {
	switch {
	case due.Before(today):
		return "overdue"
	case due.Equal(today):
		return "today"
	case due.Before(today.AddDate(0, 0, dueSoon+1)):
		return "soon"
	default:
		return "later"
	}
}

// commentView prepares one comment: who wrote it, how long ago, and whether the
// person reading it is allowed to take it away.
func (s *Server) commentView(u identity.User, c model.Comment) commentView {
	return commentView{
		Comment: c,
		Mine:    strings.EqualFold(u.Email, c.Author),
		Ago:     ago(s.now().UTC(), c.CreatedAt),
		Exact:   c.CreatedAt.Format("2 Jan 2006, 15:04") + " UTC",
	}
}

// ago renders how long ago something happened.
//
// The server does not know what timezone the reader is in — the app stores and
// shows UTC throughout — so a wall-clock time would be wrong for everyone
// outside it, while "3 hours ago" is right for everybody. The exact instant is
// still there, in the title attribute.
func ago(now, then time.Time) string {
	d := now.Sub(then)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	default:
		return then.Format("2 Jan 2006")
	}
}

// plural writes a count with its unit, so a template never has to decide
// between one card and two cards.
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// boardPage assembles everything one board's page prints: its columns, its
// cards graded against its clock, and the boards behind the switcher.
func (s *Server) boardPage(u identity.User, b *model.Board, cards []model.Card, comments map[model.ID]int) boardPage {
	p := boardPage{
		Title: b.Name, User: u, BoardSlug: b.Slug, Board: b,
		Rows:      model.LayoutOrDefault(b.Layout) == model.LayoutRows,
		EmptyCard: cardView{Viewer: u, BoardLabels: b.Labels, Columns: b.Columns},
	}
	// Read off the cards already in hand, so the board's own render — the one
	// page that is drawn constantly — costs no extra query for its menus.
	people := service.People(cards, u.Email)
	clock := b.SLA.Clock()
	byColumn := map[model.ID][]cardView{}
	for _, c := range cards {
		byColumn[c.ColumnID] = append(byColumn[c.ColumnID], s.cardView(u, b, clock, c, comments[c.ID], people))
	}
	for _, col := range b.Columns {
		cv := columnHead(col, len(byColumn[col.ID]), false)
		cv.Cards = byColumn[col.ID]
		p.Columns = append(p.Columns, cv)
	}
	return p
}

// columnHeads is every column header of b with the counts it has now, marked
// for an out-of-band swap.
//
// The responses that change a column's count carry these, so the
// column the card left is redrawn as well as the one it arrived in without the
// caller having to say which those were. The counts come from a read rather
// than from arithmetic on what the last render said, since a header that
// disagrees with the column under it is worse than a header that costs a query.
func (s *Server) columnHeads(ctx context.Context, b *model.Board) []fragment {
	cards, err := s.svc.Cards(ctx, b.ID)
	if err != nil {
		s.log.Error("column heads", "board", b.ID, "err", err)
		return nil
	}
	n := map[model.ID]int{}
	for _, c := range cards {
		n[c.ColumnID]++
	}
	// The header and the tab both carry the count, so both come back. The tab
	// strip is what a phone navigates by and it used to keep whatever number
	// the page was loaded with, disagreeing with the header until a reload.
	frags := make([]fragment, 0, 2*len(b.Columns))
	for _, col := range b.Columns {
		v := columnHead(col, n[col.ID], true)
		frags = append(frags, fragment{"columnhead", v}, fragment{"columntab", v})
	}
	return frags
}
