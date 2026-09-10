//go:generate go run gen_zones.go

package model

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// DaySet is the weekdays a board's clock runs on, one bit per time.Weekday.
//
// A bitmask rather than a list, because it is one integer in every backend and
// one row of checkboxes in the form, and because "is Wednesday an office day"
// is the only question anything ever asks of it.
type DaySet uint8

// Day is the bit for one weekday.
func Day(w time.Weekday) DaySet { return 1 << uint(w) }

// MonToFri is the office week a board gets before anyone changes it.
const MonToFri DaySet = 1<<uint(time.Monday) | 1<<uint(time.Tuesday) |
	1<<uint(time.Wednesday) | 1<<uint(time.Thursday) | 1<<uint(time.Friday)

// AllDays is a desk that is staffed every day.
const AllDays DaySet = 1<<7 - 1

// Has reports whether the clock runs on w.
func (d DaySet) Has(w time.Weekday) bool { return d&Day(w) != 0 }

// Any reports whether the clock runs at all. A set with no days in it cannot
// measure anything, so it switches the SLA off rather than hanging a search
// for the next office minute.
func (d DaySet) Any() bool { return d&AllDays != 0 }

// Weekdays returns the days in the set, Monday first, because an office week
// starting on Sunday reads as a mistake even where the calendar does.
func (d DaySet) Weekdays() []time.Weekday {
	week := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday,
		time.Friday, time.Saturday, time.Sunday}
	var out []time.Weekday
	for _, w := range week {
		if d.Has(w) {
			out = append(out, w)
		}
	}
	return out
}

// String is the set as "Mon to Fri" for a run of days and "Mon, Wed, Fri"
// otherwise, which is how an office week is written down.
func (d DaySet) String() string {
	days := d.Weekdays()
	if len(days) == 0 {
		return "never"
	}
	short := make([]string, len(days))
	for i, w := range days {
		short[i] = w.String()[:3]
	}
	// A run only reads as a range when it is one in the list above, so Saturday
	// with Sunday is a range and Sunday with Monday is two days.
	if len(days) > 2 && isRun(days) {
		return short[0] + " to " + short[len(short)-1]
	}
	return strings.Join(short, ", ")
}

// Names is the set as ["mon", "tue"], Monday first: how a day goes over the
// wire and how a form posts one back.
//
// Names rather than the bitmask, because a caller writing JSON should not have
// to know that Wednesday is 8, and rather than the numbers, because a set that
// arrives as [1, 2, 3] is unreadable in a log and one off by a day either way
// looks the same as a correct one.
func (d DaySet) Names() []string {
	days := d.Weekdays()
	out := make([]string, len(days))
	for i, w := range days {
		out[i] = dayName(w)
	}
	return out
}

func dayName(w time.Weekday) string { return strings.ToLower(w.String()[:3]) }

// ErrBadDay is returned by ParseDays for a name that is not a weekday.
var ErrBadDay = errors.New("not a weekday")

// ParseDays reads a list of day names into a set. "mon" and "Monday" are the
// same day; anything else is ErrBadDay. An empty list is the empty set, which
// is a promise that never runs rather than an error.
func ParseDays(names []string) (DaySet, error) {
	var out DaySet
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if len(n) < 3 {
			return 0, fmt.Errorf("%q: %w", n, ErrBadDay)
		}
		found := false
		for w := time.Sunday; w <= time.Saturday; w++ {
			full := strings.ToLower(w.String())
			if n == full || n == full[:3] {
				out |= Day(w)
				found = true
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("%q: %w", n, ErrBadDay)
		}
	}
	return out, nil
}

// ClockString renders minutes since midnight as "08:00".
//
// The end of a day is MinutesPerDay, which has no reading on a 24-hour clock
// and no place in an <input type="time">, so it goes back out as the midnight
// it is. ParseClock reads it back the same way round.
func ClockString(m int) string {
	if m >= MinutesPerDay {
		return "00:00"
	}
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

// Zones is the time-zone picker's list: every region, and inside each one the
// names sorted. When current is a zone the list does not carry, it comes back
// as a group of its own at the front rather than being dropped, because a board
// whose zone was set through the API must not lose it the next time somebody
// saves the form.
func Zones(current string) []ZoneGroup {
	if current == "" || current == "UTC" {
		return zoneGroups
	}
	for _, g := range zoneGroups {
		if slices.Contains(g.Zones, current) {
			return zoneGroups
		}
	}
	out := make([]ZoneGroup, 0, len(zoneGroups)+1)
	out = append(out, ZoneGroup{Region: "Set on this board", Zones: []string{current}})
	return append(out, zoneGroups...)
}

// ErrBadClock is returned by ParseClock for anything that is not a time of day.
var ErrBadClock = errors.New("not a time of day, such as 08:00")

// ParseClock reads "08:00" as minutes since midnight. With endOfDay set,
// "00:00" is the midnight that ends the day rather than the one that starts it,
// so a desk staffed around the clock is 00:00 to 00:00.
func ParseClock(s string, endOfDay bool) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, ErrBadClock
	}
	m := t.Hour()*60 + t.Minute()
	if m == 0 && endOfDay {
		return MinutesPerDay, nil
	}
	return m, nil
}

// isRun reports whether the days are consecutive in Monday-first order.
func isRun(days []time.Weekday) bool {
	order := map[time.Weekday]int{time.Monday: 0, time.Tuesday: 1, time.Wednesday: 2,
		time.Thursday: 3, time.Friday: 4, time.Saturday: 5, time.Sunday: 6}
	for i := 1; i < len(days); i++ {
		if order[days[i]] != order[days[i-1]]+1 {
			return false
		}
	}
	return true
}

// Office hours as minutes since midnight, for the defaults and the form.
const (
	// MinutesPerDay is one past the last minute an office day can end at.
	MinutesPerDay = 24 * 60
	defaultStart  = 8 * 60
	defaultEnd    = 17 * 60
)

// MaxResponseHours caps the promise. Beyond a few office weeks a response time
// says nothing anybody will act on, and the cap is also what keeps the walk
// over the calendar in Deadline short.
const MaxResponseHours = 2000

// SLA is a board's response-time promise: how long a card may sit untouched
// during office hours before the board says so.
//
// It is deliberately one promise per board rather than one per column or per
// label. A support desk has a response time, not a matrix of them, and a board
// that needed a matrix would need a priority field first.
type SLA struct {
	// ResponseHours is the promise, counted in office hours rather than in
	// hours on the wall. Zero is the whole thing switched off, which is what
	// every board has until somebody sets one.
	ResponseHours int
	// Days are the weekdays the clock runs on.
	Days DaySet
	// Start and End are the office hours on such a day, as minutes since
	// midnight in Zone, half-open: a day ends at End and End is not part of
	// it. Minutes rather than a time.Time, because a clock reading with no
	// date is not an instant and storing one invites a timestamp in year 1.
	Start, End int
	// Zone is an IANA name such as Europe/Zurich; empty is UTC. The office
	// hours of a desk are local hours, so a clock that ran in UTC would open
	// an hour late for half the year.
	Zone string
}

// DefaultSLA is what the form offers a board that has never set one: the
// office week, 08:00 to 17:00, and the clock switched off.
func DefaultSLA() SLA {
	return SLA{Days: MonToFri, Start: defaultStart, End: defaultEnd}
}

// Enabled reports whether the SLA can measure anything. A promise of zero
// hours, a week with no office days and a day that ends before it starts all
// mean the same thing to a card: no badge, no deadline.
func (s SLA) Enabled() bool {
	return s.ResponseHours > 0 && s.Days.Any() && s.Start < s.End && s.End <= MinutesPerDay
}

// Window is the promise as a duration, for grading how much of it is left.
func (s SLA) Window() time.Duration { return time.Duration(s.ResponseHours) * time.Hour }

// Clean brings an SLA into the range every backend accepts: a promise of at
// most MaxResponseHours, days that exist, and office hours inside one day.
//
// This is the SLA's LayoutOrDefault, applied on write by all three stores, so
// that the SQL backends and the in-memory one hold the same thing and a value
// written by something that did not validate cannot fail a CHECK constraint.
// A day that ends before it starts is left alone: it is in range, and
// Enabled reads it as the promise being off, which is the honest answer.
func (s SLA) Clean() SLA {
	s.ResponseHours = min(max(s.ResponseHours, 0), MaxResponseHours)
	s.Days &= AllDays
	s.Start = min(max(s.Start, 0), MinutesPerDay)
	s.End = min(max(s.End, 0), MinutesPerDay)
	return s
}

// Clock is an SLA with its time zone resolved.
//
// Resolving one costs a read of the zone database, so a board measuring twenty
// cards resolves it once and measures with this. A Clock is a value: copying
// it is free and it holds no state between calls.
type Clock struct {
	SLA SLA
	loc *time.Location
}

// Clock resolves the zone, falling back to UTC when it is empty or unknown.
//
// An unknown zone falls back rather than failing, because the alternative is a
// board that will not render after somebody typos a zone name, and because the
// zone is the least of what the SLA says.
func (s SLA) Clock() Clock {
	loc := time.UTC
	if s.Zone != "" {
		if l, err := time.LoadLocation(s.Zone); err == nil {
			loc = l
		}
	}
	return Clock{SLA: s, loc: loc}
}

// Location is the zone the clock runs in, never nil.
func (c Clock) Location() *time.Location {
	if c.loc == nil {
		return time.UTC
	}
	return c.loc
}

// Enabled reports whether this clock measures anything; see SLA.Enabled.
func (c Clock) Enabled() bool { return c.SLA.Enabled() }

// maxSteps bounds the two walks over the calendar. A step is one office day,
// an office day is a whole number of minutes long, and the promise is capped
// at MaxResponseHours, so Deadline reaches its answer inside this many steps
// even for the shortest office day the form accepts. Between takes the same
// bound, which is why a card carrying a timestamp from another century reports
// the office time of the years it walked rather than walking all of them.
const maxSteps = MaxResponseHours * 60

// window is the office hours of t's own calendar day, whether or not that day
// is an office day. The end is exclusive.
func (c Clock) window(t time.Time) (start, end time.Time) {
	loc := c.Location()
	y, m, d := t.In(loc).Date()
	// The minutes go in the minute field and time.Date normalises them, which
	// is also what makes an hour that a DST change skipped resolve to a real
	// instant instead of a panic.
	return time.Date(y, m, d, 0, c.SLA.Start, 0, 0, loc), time.Date(y, m, d, 0, c.SLA.End, 0, 0, loc)
}

// next is the first office instant at or after t. t itself when the clock is
// already running.
//
// A set with any day in it is reached inside a week, so the walk is over the
// day t lands on and the seven that follow. A set with none has no office
// instant to find, and t is the honest answer to that.
func (c Clock) next(t time.Time) time.Time {
	loc := c.Location()
	t = t.In(loc)
	if !c.SLA.Days.Any() {
		return t
	}
	for range 8 {
		if c.SLA.Days.Has(t.Weekday()) {
			start, end := c.window(t)
			if t.Before(start) {
				return start
			}
			if t.Before(end) {
				return t
			}
		}
		y, m, d := t.Date()
		t = time.Date(y, m, d+1, 0, 0, 0, 0, loc)
	}
	return t
}

// Deadline is when a card last touched at `from` breaches, counting only
// office time. Zero when the board has no SLA.
func (c Clock) Deadline(from time.Time) time.Time {
	if !c.Enabled() {
		return time.Time{}
	}
	left := c.SLA.Window()
	t := c.next(from)
	for range maxSteps {
		_, end := c.window(t)
		room := end.Sub(t)
		if room >= left {
			return t.Add(left)
		}
		if room <= 0 {
			// Enabled has Start before End, so an office day holds something.
			// Only a zone whose offset jumped the whole window gets here, and
			// a walk that cannot move on stops where it is.
			return t
		}
		left -= room
		// end is one past its own day, so this lands on the next office day.
		t = c.next(end)
	}
	return t
}

// Between is the office time in [a, b), zero when b is not after a.
func (c Clock) Between(a, b time.Time) time.Duration {
	if !c.Enabled() || !b.After(a) {
		return 0
	}
	var total time.Duration
	t := c.next(a)
	for range maxSteps {
		if !t.Before(b) {
			return total
		}
		_, end := c.window(t)
		if !end.After(t) {
			return total
		}
		stop := end
		if stop.After(b) {
			stop = b
		}
		total += stop.Sub(t)
		t = c.next(end)
	}
	return total
}

// How a card stands against the promise. The empty state is nobody waiting,
// which is what a front-end draws as no badge at all.
const (
	SLAOff      = ""
	SLAOK       = "ok"
	SLASoon     = "soon"
	SLABreached = "breached"
)

// soonPart is the share of the window at which a card starts warning. A quarter
// is late enough that most cards never warn and early enough that a four-hour
// promise gives an hour's notice.
const soonPart = 4

// CardState grades one card against the promise, and says how much office time
// is left of it; the duration is negative once the promise has gone by.
//
// SLAOff, and a zero duration, when nobody is waiting: no promise on the board,
// a card that has left it, or a column that stops the clock. That last one is
// what keeps a Done column from turning red, and it is also why this takes the
// column rather than only the card.
func (c Clock) CardState(card Card, col *Column, now time.Time) (string, time.Duration) {
	if !c.Enabled() || card.Archived() || (col != nil && col.StopsClock) {
		return SLAOff, 0
	}
	left := c.Left(card.UpdatedAt, now)
	switch {
	case left <= 0:
		return SLABreached, left
	case left <= c.SLA.Window()/soonPart:
		return SLASoon, left
	default:
		return SLAOK, left
	}
}

// Left is the office time between now and the deadline of a card last touched
// at `from`, negative once the deadline has gone by. Zero when there is no SLA.
//
// Office time rather than wall time on both sides of the deadline: a card that
// breached on Friday afternoon is two office hours late on Monday morning, not
// three days late, and telling a desk it is three days late for a weekend it
// was never open is how a badge stops being read.
func (c Clock) Left(from, now time.Time) time.Duration {
	deadline := c.Deadline(from)
	if deadline.IsZero() {
		return 0
	}
	if now.Before(deadline) {
		return c.Between(now, deadline)
	}
	return -c.Between(deadline, now)
}
