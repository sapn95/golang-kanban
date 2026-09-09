# 0012 — One JSON snapshot is the portable format, and S3 is where it goes

## Status

Accepted.

## Context

There are three backends and no way to get data from one into another. Moving a
board from the SQLite file on a laptop to the Postgres in the cluster means a
`pg_dump` that only Postgres reads, or SQL written by hand against two schemas
that are allowed to differ. The same gap is why there is no backup: `pg_dump`
in a sidecar backs up Postgres, not the board, and it backs up nothing at all
when the board is the SQLite file the container has mounted.

The roadmap answered a different question. It listed `store/mongo` and
`store/s3` as further backends, which reads as though the way to get a board
into a bucket is to teach the board to live there. A bucket cannot do what
`store.Store` asks for: no transaction around a card move, no unique index on a
slug, no read that is guaranteed to see the write before it. Every one of the
guarantees in [0003](0003-store-interface-and-portable-ids.md) would become a
read of the whole object, a merge in memory and a write back, with the last
writer winning and nobody told.

What I wanted from the bucket was never a database. It was a copy of the board
somewhere that is not the machine the board runs on.

## Decision

One document, `internal/backup`, written on top of `store.Store` and therefore
once for every backend. `kanban export` writes it, `kanban import` reads it,
and a schedule in the serving process writes it to a directory or a bucket on a
timer.

### The format

A snapshot is a `format` number, the instant it was taken, the version of the
binary that wrote it, and the boards, each with its columns, labels and cards,
each card with its labels, subtasks and comments. The document is not streamed:
it is built in memory, because `Import` checks the whole of it before the first
write, and a board too large for the memory of the process could not be drawn
either.

Two things it deliberately leaves out.

**No positions.** The order of the arrays is the order of the board. A file
that also carried `position: 3` could contradict itself, and people do edit
these by hand. `Import` replays the arrays in order and the store numbers them,
which is what makes the round trip a property worth testing: export, import,
export gives a byte-identical document across every pair of backends.

**No renumbering.** Card and comment IDs come back as they were, because they
are portable by construction (0003) and a restore that reassigned them would
break every link anyone had saved.

The one thing the round trip cannot show is the archived cards, so it is stated
here: an archived card is archived again on import and lands at the end of its
column, not in the row it was in when it was archived. The board never showed
that row, and reserving a slot for a card nobody can see is not worth a
`position` field in the file.

`format` is `1`. A snapshot from a later version is refused with a sentence
rather than half understood, and the number goes up when a change alters what
`Import` has to do, not when a field is added that an older importer can skip.

### S3 is a target, not a backend

`store/mongo` and `store/s3` are dropped from the roadmap.

S3 gets what it is good at: a key with a timestamp in it, written once, listed,
and deleted when it is old enough. `BACKUP_S3_BUCKET` points at a bucket,
`BACKUP_S3_ENDPOINT` at MinIO or Garage or anything else that speaks the same
protocol, and `BACKUP_DIR` at a directory for a deployment whose off-machine
copy is somebody else's job. One target at a time, because two would need two
retention policies and there is one `BACKUP_KEEP`.

Names are `kanban-20260304T050607Z.json`, so lexical order is chronological
order and retention needs to read no files. Retention runs after the write, so
a target that will not let go of the old snapshots is an error beside a fresh
one rather than a lost backup. The schedule takes a snapshot on start unless the
target already holds one from within the interval, which is what keeps a
frequently redeployed container backing up without letting a restart loop fill
the bucket.

MongoDB gets nothing until someone says what they want from it that Postgres
does not do. It was on the roadmap because the original project had a Mongo
branch, which is not a reason.

### The signature is hand-rolled

`internal/backup/s3.go` signs its own requests. The AWS SDK for Go is around
forty modules, and this package makes four calls: put an object, list them,
delete one, and that is the whole surface. Under the rule that every dependency
costs an ADR ([0001](0001-package-layout-and-single-static-binary.md)), forty
modules for four calls is the more expensive answer, and it would land in a
binary whose selling point is that it has no supply chain to speak of.

Signature Version 4 is documented and has not changed since 2012. The
implementation is about a hundred lines, and it is only trustworthy because of
how it is checked: the three signatures pinned in `s3_test.go` were produced by
botocore for the same requests, and that harness was itself validated against
the `get-vanilla` vector AWS publishes before any of its output was written
down. A test that only agrees with the code it tests would have caught nothing.

What this gives up is the SDK's credential chain. There is no
`~/.aws/credentials`, no instance metadata, no `AssumeRole`, no IRSA. The keys
come from `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and, for temporary
credentials somebody else refreshes, `AWS_SESSION_TOKEN`, under their usual
names so a deployment that already injects them does not need a second set.

## Consequences

There is now a supported way to move a board between backends and between
deployments, and it is the same file that lands in the bucket at midnight. It
is also a way to lose a board: `kanban import -replace` deletes the boards it
is about to write, which is why an existing board without that flag is a
refusal that names the flag, and why `-dry-run` reports what a file holds
without touching the database.

The file is every card, every comment and every assignee's address in plain
text. `kanban export -o` writes `0600` and so does the directory target, but a
bucket is as private as its policy, and a snapshot in an S3 bucket that allows
public reads is the whole board published.

Snapshots are the only thing the format promises. A restore brings back boards
as they were and nothing that was never in a board: no schema version, no
migration history, no `AUTO_MIGRATE` state. Importing into an empty database
still needs the migrations to have run.

Whoever holds a snapshot from a future version can read the JSON with any tool
they like, and this build will still refuse to import it. That is the intended
trade, and the way out of it is to keep `format` stable.

Every deployment that turns the schedule on pays for it in read load: one
snapshot is one query per board plus one per card that carries comments. At the
default of one a day that is nothing, and `BACKUP_INTERVAL` refuses anything
under a minute so that it stays nothing.
