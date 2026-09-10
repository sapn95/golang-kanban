# 0013 — A response time is counted in office hours

## Status

Accepted.

## Context

The board grades a due date, which answers "when is this wanted". A support desk
has a second question: how long a card may sit there before somebody has replied
to it. That is not a date on the card, it is a promise the board makes, and it is
the same promise for every card on it.

Counting it in wall-clock hours does not work for a desk that is staffed office
hours. A card that arrives at 16:50 on Friday with four hours to answer is not
late at 20:50 that evening, and a board that said so would be ignored by the
Monday morning shift, which is exactly when it should be read. So the count has
to run in the desk's own hours, which means the board has to know which days it
is open, between which hours, and in which zone.

Two more things follow from a real desk. Some columns are not a wait: a card in
Done or in Waiting for the customer is nobody's response time, and a clock still
running there is a red badge nobody can clear. And "untouched" has to mean
something the board can see, so the clock reads the card's `updated_at`: an edit,
a move, an assignee, a comment all touch the card and start it again.

The alternative was a per-card timer, the shape most trackers use. It carries
more: a start, a pause, an accumulated total, and a write on every transition. It
buys a card that can be paused by hand, which is a desk feature, not a board one,
and it means the response time of a card can be edited, which is the thing an SLA
exists to prevent.

## Decision

One promise per board, held as `model.SLA{ResponseHours, Days, Start, End, Zone}`
and evaluated on read.

- `Days` is a bitmask over `time.Weekday`, `Start` and `End` are minutes since
  midnight, half-open, so an office day is `[Start, End)`. Zero hours is the
  clock off, which is what a board has until somebody sets one.
- `model.Clock` is the promise with its `*time.Location` resolved once. It walks
  one calendar day at a time in that zone, so daylight saving moves the wall
  clock and leaves the office hours where they are: 08:00 to 17:00 is eight hours
  in March and eight hours in November.
- An `<input type="time">` has no 24:00, so an `End` of `"00:00"` is the midnight
  that ends the day, 1440 minutes in, and a desk staffed around the clock is
  00:00 to 00:00. `model.ParseClock(s, endOfDay bool)` is where that is decided,
  once, for the form and the API both.
- The state is computed from `card.UpdatedAt`, never stored: `off`, `ok`, `soon`
  in the last quarter of the promise, `breached` past it. Nothing writes to a
  card as its clock runs, so a board with a promise has the same write pattern as
  one without, and a promise that changes regrades every card at once.
- A column can carry `stops_clock`. A card in one has no badge, and the archive
  has none either, because a card nobody is waiting on is not late.
- Day names and `HH:MM` readings are the representation everywhere: the HTML
  form, the JSON API, and the snapshot document all go through
  `model.ParseDays`, `model.ParseClock`, `DaySet.Names()` and
  `model.ClockString`, so a day cannot mean Tuesday in a form and Wednesday in a
  script.
- `cmd/kanban` imports `_ "time/tzdata"`. Without it `time.LoadLocation` falls
  back to UTC on a host with no `/usr/share/zoneinfo`, which is most containers,
  and every badge would be an hour or two out with nothing on the page saying
  why. Half a megabyte to make the zone name mean what it says.
- The zone is checked by the service before it is written, and an end before its
  start is refused rather than stored: `SLA.Enabled` would read that as off, and
  a promise that quietly stopped promising is worse than a form that would not
  accept it.

## Consequences

- A board answers "which cards are we late on" from the card faces, with no
  background job, no timer and no extra column to keep in step.
- The badge is derived, so it is only ever as right as `updated_at`. A card
  touched for a typo looks answered. That is the trade for having no per-card
  timer, and it is the same trade the WIP limit makes by counting cards rather
  than tracking flow.
- Response time is a board-wide promise. A desk that owes one customer four hours
  and another two needs two boards, and the settings page says as much rather
  than offering a per-card override that the model does not have.
- The badge does not refresh on its own. A card goes amber when the page is next
  drawn, which is what a board that loads in one request costs, and htmx redraws
  a card on every edit anyway.
- `search` has no `sla:` term yet. The state is computed per card against the
  board's clock, so a filter is a pass over the column rather than a predicate
  the store can push down; it is worth having, and it is not this change.
