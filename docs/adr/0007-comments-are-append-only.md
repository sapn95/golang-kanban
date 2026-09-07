# 0007 — Comments are append-only

## Status

Accepted.

## Context

Cards now carry a discussion. The identity layer from ADR 0005 means a comment
can name who wrote it, which is what makes a thread worth keeping at all.

That raises the question the feature cannot avoid: can a comment be changed
after it is posted?

An editable comment is only worth as much as the record of it having changed.
Without one, anyone who can edit can rewrite what they said after the fact, and
the thread stops being evidence of anything. Keeping that record means storing
revisions, showing an "edited" marker, and deciding who may see the previous
text. That is a real feature with its own schema, and this board has no user
table to hang any of it on.

Deleting is a smaller question but the same shape. A comment posted by mistake
is a real case, and there is nothing else on a card that cannot be undone.

## Decision

A comment can be written and it can be removed by the person who wrote it.
There is no edit.

Removal leaves nothing behind. A tombstone reading "comment deleted" only means
something where the reader does not already know who else is on the board; on
a board with two people it is noise dressed as an audit trail.

The `comments` table has no `updated_at`, which is the schema saying the same
thing: a row never changes after it is written.

Authorship is compared case-insensitively against the address the identity
layer supplies. Two empty addresses count as a match. That is not a hole — an
empty author can only occur where `AUTH_MODE=none`, and there the deployment
has exactly one user by definition. Behind Cloudflare Access every comment
carries a real address and the comparison is between two of them.

## Consequences

A typo stays a typo unless the author removes the comment and writes it again,
which is visible in the thread's ordering rather than hidden.

Nothing needs to explain, on screen or in the schema, why an old version of a
comment is or is not visible to a given reader.

If the board ever grows a user table and an audit log of its own, editing
becomes a question worth reopening. Until then, adding it would produce a
record nobody could trust.
