package model

import (
	"slices"
	"testing"
	"time"
)

// zurich is the zone the arithmetic is worth testing in: it changes offset
// twice a year, which is what a clock counting office hours has to survive.
func zurich(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Skipf("no zone database: %v", err)
	}
	return loc
}

func TestDaySet(t *testing.T) {
	cases := []struct {
		name string
		days DaySet
		want string
	}{
		{"the office week", MonToFri, "Mon to Fri"},
		{"every day", AllDays, "Mon to Sun"},
		{"a run of three", Day(time.Monday) | Day(time.Tuesday) | Day(time.Wednesday), "Mon to Wed"},
		{"two days are listed, not ranged", Day(time.Monday) | Day(time.Tuesday), "Mon, Tue"},
		{"a gap is listed", Day(time.Monday) | Day(time.Wednesday) | Day(time.Friday), "Mon, Wed, Fri"},
		{"the weekend is a run", Day(time.Friday) | Day(time.Saturday) | Day(time.Sunday), "Fri to Sun"},
		{"Sunday to Monday is not", Day(time.Sunday) | Day(time.Monday) | Day(time.Tuesday), "Mon, Tue, Sun"},
		{"no days at all", 0, "never"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.days.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
	if !MonToFri.Has(time.Wednesday) || MonToFri.Has(time.Saturday) {
		t.Error("MonToFri has the wrong days in it")
	}
	if DaySet(0).Any() {
		t.Error("an empty set says it has days")
	}
}

func TestSLAEnabled(t *testing.T) {
	on := SLA{ResponseHours: 4, Days: MonToFri, Start: 8 * 60, End: 17 * 60}
	cases := []struct {
		name string
		sla  SLA
		want bool
	}{
		{"a promise, a week and a day", on, true},
		{"no promise", SLA{Days: MonToFri, Start: 8 * 60, End: 17 * 60}, false},
		{"no office days", SLA{ResponseHours: 4, Start: 8 * 60, End: 17 * 60}, false},
		{"a day that ends when it starts", SLA{ResponseHours: 4, Days: MonToFri, Start: 9 * 60, End: 9 * 60}, false},
		{"a day that ends before it starts", SLA{ResponseHours: 4, Days: MonToFri, Start: 17 * 60, End: 8 * 60}, false},
		{"an end past midnight", SLA{ResponseHours: 4, Days: MonToFri, Start: 8 * 60, End: MinutesPerDay + 1}, false},
		{"the default, which is off", DefaultSLA(), false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.sla.Enabled(); got != tt.want {
				t.Errorf("Enabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClockZoneFallsBackToUTC(t *testing.T) {
	if got := (SLA{Zone: "Mars/Olympus"}).Clock().Location(); got != time.UTC {
		t.Errorf("an unknown zone resolved to %v, want UTC", got)
	}
	if got := (SLA{}).Clock().Location(); got != time.UTC {
		t.Errorf("an empty zone resolved to %v, want UTC", got)
	}
	// The zero Clock is what a caller gets from a board nobody configured.
	if got := (Clock{}).Location(); got != time.UTC {
		t.Errorf("the zero clock resolved to %v, want UTC", got)
	}
	if (Clock{}).Enabled() {
		t.Error("the zero clock says it measures something")
	}
	if d := (Clock{}).Deadline(time.Now()); !d.IsZero() {
		t.Errorf("the zero clock gave a deadline %v", d)
	}
}

func TestClockDeadline(t *testing.T) {
	loc := zurich(t)
	// Mon to Fri, 08:00 to 17:00, so nine office hours a day.
	desk := SLA{ResponseHours: 4, Days: MonToFri, Start: 8 * 60, End: 17 * 60, Zone: "Europe/Zurich"}
	at := func(y int, m time.Month, d, h, min int) time.Time {
		return time.Date(y, m, d, h, min, 0, 0, loc)
	}
	cases := []struct {
		name  string
		hours int
		from  time.Time
		want  time.Time
	}{
		{
			"inside office hours it is four hours later",
			4, at(2026, time.September, 10, 9, 0), at(2026, time.September, 10, 13, 0),
		},
		{
			"before the desk opens the clock starts at opening",
			4, at(2026, time.September, 10, 5, 30), at(2026, time.September, 10, 12, 0),
		},
		{
			"after it closes the clock starts the next morning",
			2, at(2026, time.September, 10, 22, 0), at(2026, time.September, 11, 10, 0),
		},
		{
			"an evening on Friday lands on Monday",
			2, at(2026, time.September, 11, 18, 0), at(2026, time.September, 14, 10, 0),
		},
		{
			// 8 office hours are left of the Monday, so the other 4 are Tuesday
			// morning: a day is nine hours and the promise counts none of the
			// evening in between.
			"a promise longer than a day carries over",
			12, at(2026, time.September, 14, 9, 0), at(2026, time.September, 15, 12, 0),
		},
		{
			// One hour of the Friday, then the whole Monday.
			"a promise that crosses the weekend",
			10, at(2026, time.September, 11, 16, 0), at(2026, time.September, 14, 17, 0),
		},
		{
			"a whole office day ends exactly at closing",
			9, at(2026, time.September, 10, 8, 0), at(2026, time.September, 10, 17, 0),
		},
		{
			"a Saturday is not an office day at all",
			1, at(2026, time.September, 12, 10, 0), at(2026, time.September, 14, 9, 0),
		},
		{
			// Zurich puts the clocks back on 25 October 2026, so the Sunday in
			// the middle is 25 hours long and the office hours are unmoved.
			"the office hours do not move when the clocks do",
			4, at(2026, time.October, 23, 16, 0), at(2026, time.October, 26, 11, 0),
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			sla := desk
			sla.ResponseHours = tt.hours
			got := sla.Clock().Deadline(tt.from)
			if !got.Equal(tt.want) {
				t.Errorf("Deadline(%s) = %s, want %s",
					tt.from.Format(time.RFC3339), got.Format(time.RFC3339), tt.want.Format(time.RFC3339))
			}
		})
	}
}

func TestClockBetween(t *testing.T) {
	loc := zurich(t)
	desk := SLA{ResponseHours: 4, Days: MonToFri, Start: 8 * 60, End: 17 * 60, Zone: "Europe/Zurich"}
	at := func(d, h, min int) time.Time { return time.Date(2026, time.September, d, h, min, 0, 0, loc) }
	cases := []struct {
		name string
		a, b time.Time
		want time.Duration
	}{
		{"an hour inside the day", at(10, 9, 0), at(10, 10, 0), time.Hour},
		{"an evening counts for nothing", at(10, 18, 0), at(10, 23, 0), 0},
		{"backwards is nothing, not a negative", at(10, 12, 0), at(10, 9, 0), 0},
		{"the same instant is nothing", at(10, 9, 0), at(10, 9, 0), 0},
		{"only the office part of a whole day", at(10, 0, 0), at(11, 0, 0), 9 * time.Hour},
		{"a weekend counts for nothing", at(12, 0, 0), at(14, 0, 0), 0},
		{"Friday afternoon to Monday morning", at(11, 16, 0), at(14, 9, 0), 2 * time.Hour},
		{"a whole office week", at(7, 0, 0), at(12, 0, 0), 45 * time.Hour},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := desk.Clock().Between(tt.a, tt.b); got != tt.want {
				t.Errorf("Between = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClockLeft(t *testing.T) {
	loc := zurich(t)
	desk := SLA{ResponseHours: 4, Days: MonToFri, Start: 8 * 60, End: 17 * 60, Zone: "Europe/Zurich"}
	at := func(d, h, min int) time.Time { return time.Date(2026, time.September, d, h, min, 0, 0, loc) }
	cases := []struct {
		name      string
		from, now time.Time
		want      time.Duration
	}{
		{"untouched for an hour of a four hour promise", at(10, 9, 0), at(10, 10, 0), 3 * time.Hour},
		{"nothing left at the deadline itself", at(10, 9, 0), at(10, 13, 0), 0},
		{"an hour over", at(10, 9, 0), at(10, 14, 0), -time.Hour},
		{
			// Touched Friday at 16:00, so the deadline is Monday at 11:00 and
			// the weekend in between is not time the desk had.
			"the weekend is not time over", at(11, 16, 0), at(14, 9, 0), 2 * time.Hour,
		},
		{"over the weekend and into Monday", at(11, 16, 0), at(14, 12, 0), -time.Hour},
		{"the whole promise is still there", at(10, 9, 0), at(10, 9, 0), 4 * time.Hour},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := desk.Clock().Left(tt.from, tt.now); got != tt.want {
				t.Errorf("Left = %v, want %v", got, tt.want)
			}
		})
	}
	if got := (SLA{}).Clock().Left(at(10, 9, 0), at(10, 12, 0)); got != 0 {
		t.Errorf("a board with no SLA has %v left, want 0", got)
	}
}

func TestClockCardState(t *testing.T) {
	loc := zurich(t)
	desk := SLA{ResponseHours: 4, Days: MonToFri, Start: 8 * 60, End: 17 * 60, Zone: "Europe/Zurich"}
	at := func(d, h, min int) time.Time { return time.Date(2026, time.September, d, h, min, 0, 0, loc) }
	now := at(10, 12, 0)
	todo := &Column{Name: "To Do"}
	done := &Column{Name: "Done", StopsClock: true}
	cases := []struct {
		name  string
		card  Card
		col   *Column
		sla   SLA
		want  string
		wantD time.Duration
	}{
		{"touched an hour ago", Card{UpdatedAt: at(10, 11, 0)}, todo, desk, SLAOK, 3 * time.Hour},
		{"an hour of four left", Card{UpdatedAt: at(10, 9, 0)}, todo, desk, SLASoon, time.Hour},
		// Touched Wednesday at 16:00, so the deadline was Thursday at 11:00.
		{"the promise has gone by", Card{UpdatedAt: at(9, 16, 0)}, todo, desk, SLABreached, -time.Hour},
		{"exactly at the deadline is over it", Card{UpdatedAt: at(10, 8, 0)}, todo, desk, SLABreached, 0},
		{"the clock stops in Done", Card{UpdatedAt: at(10, 8, 0)}, done, desk, SLAOff, 0},
		{
			"an archived card is nobody's wait",
			Card{UpdatedAt: at(10, 8, 0), ArchivedAt: at(10, 9, 0)}, todo, desk, SLAOff, 0,
		},
		{"a board with no promise", Card{UpdatedAt: at(10, 8, 0)}, todo, SLA{}, SLAOff, 0},
		{"no column to ask about", Card{UpdatedAt: at(10, 11, 0)}, nil, desk, SLAOK, 3 * time.Hour},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			state, left := tt.sla.Clock().CardState(tt.card, tt.col, now)
			if state != tt.want || left != tt.wantD {
				t.Errorf("CardState = %q, %v, want %q, %v", state, left, tt.want, tt.wantD)
			}
		})
	}
}

func TestSLAClean(t *testing.T) {
	got := SLA{ResponseHours: -5, Days: 255, Start: -1, End: MinutesPerDay + 90}.Clean()
	want := SLA{ResponseHours: 0, Days: AllDays, Start: 0, End: MinutesPerDay}
	if got != want {
		t.Errorf("Clean() = %+v, want %+v", got, want)
	}
	over := SLA{ResponseHours: MaxResponseHours + 1}.Clean()
	if over.ResponseHours != MaxResponseHours {
		t.Errorf("Clean() kept %d hours, want %d", over.ResponseHours, MaxResponseHours)
	}
	desk := SLA{ResponseHours: 4, Days: MonToFri, Start: 8 * 60, End: 17 * 60, Zone: "Europe/Zurich"}
	if desk.Clean() != desk {
		t.Errorf("Clean() changed a valid SLA: %+v", desk.Clean())
	}
}

// A desk that is open every day and around the clock is the one case where
// office time and wall time are the same thing, which makes it a good check
// that the walk over the calendar is not losing or inventing minutes.
func TestClockOpenAllTheTime(t *testing.T) {
	desk := SLA{ResponseHours: 30, Days: AllDays, Start: 0, End: MinutesPerDay}
	from := time.Date(2026, time.September, 10, 13, 0, 0, 0, time.UTC)
	want := from.Add(30 * time.Hour)
	if got := desk.Clock().Deadline(from); !got.Equal(want) {
		t.Errorf("Deadline = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got := desk.Clock().Between(from, from.Add(50*time.Hour)); got != 50*time.Hour {
		t.Errorf("Between = %v, want 50h", got)
	}
}

// A promise that needs more office days than a round-number bound would allow.
// Fifteen minutes is a legal office day and 2000 hours is the largest promise
// the model takes, so this one comes due eight thousand office days out, and a
// walk that gave up before then would answer with the day it stopped on.
func TestClockDeadlineOnAVeryShortOfficeDay(t *testing.T) {
	desk := SLA{ResponseHours: MaxResponseHours, Days: MonToFri,
		Start: 8 * 60, End: 8*60 + 15, Zone: "Europe/Zurich"}
	c := desk.Clock()
	from := time.Date(2026, time.September, 10, 9, 0, 0, 0, c.Location())
	deadline := c.Deadline(from)
	if got := c.Between(from, deadline); got != desk.Window() {
		t.Errorf("office time up to the deadline = %v, want the whole promise %v", got, desk.Window())
	}
	// Eight thousand office days, five to the week, is past 2050.
	if deadline.Year() < 2050 {
		t.Errorf("Deadline = %s, too early for 2000 hours at a quarter of an hour a day",
			deadline.Format(time.RFC3339))
	}
}

// A week with no office day in it has no office instant to walk to, so nothing
// searches for one.
func TestClockWithNoOfficeDay(t *testing.T) {
	desk := SLA{ResponseHours: 4, Days: 0, Start: 8 * 60, End: 17 * 60}
	c := desk.Clock()
	at := time.Date(2026, time.September, 10, 9, 0, 0, 0, time.UTC)
	if got := c.next(at); !got.Equal(at) {
		t.Errorf("next = %s, want the instant it was given", got.Format(time.RFC3339))
	}
	if got := c.Deadline(at); !got.IsZero() {
		t.Errorf("Deadline = %s, want no deadline at all", got.Format(time.RFC3339))
	}
}

// The picker must not offer a name the server would then refuse, which is the
// one way a generated list can be wrong in a way nobody notices until a board
// will not save.
func TestEveryZoneOnOfferLoads(t *testing.T) {
	seen := map[string]string{}
	n := 0
	for _, g := range Zones("") {
		if len(g.Zones) == 0 {
			t.Errorf("region %q offers nothing", g.Region)
		}
		if !slices.IsSorted(g.Zones) {
			t.Errorf("region %q is not sorted, so the list reads at random", g.Region)
		}
		for _, name := range g.Zones {
			if was, dup := seen[name]; dup {
				t.Errorf("%s is offered twice, in %q and %q", name, was, g.Region)
			}
			seen[name] = g.Region
			if _, err := time.LoadLocation(name); err != nil {
				t.Errorf("LoadLocation(%q): %v", name, err)
			}
			n++
		}
	}
	if n != ZoneCount {
		t.Errorf("walked %d zones, ZoneCount says %d", n, ZoneCount)
	}
	if n < 300 {
		t.Errorf("only %d zones; the list has lost most of itself", n)
	}
}

// A zone the list does not carry is kept rather than dropped, so a board set up
// through the API does not lose its zone the next time somebody saves the form.
func TestAZoneFromSomewhereElseStaysOnOffer(t *testing.T) {
	groups := Zones("US/Eastern")
	if len(groups) != len(zoneGroups)+1 {
		t.Fatalf("groups = %d, want one more than the %d generated", len(groups), len(zoneGroups))
	}
	if got := groups[0].Zones; len(got) != 1 || got[0] != "US/Eastern" {
		t.Errorf("first group = %v, want the board's own zone", got)
	}
	for _, current := range []string{"", "UTC", "Europe/Zurich"} {
		if len(Zones(current)) != len(zoneGroups) {
			t.Errorf("Zones(%q) added a group for a zone that is already on offer", current)
		}
	}
}
