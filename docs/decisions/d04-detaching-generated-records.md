# D4 — Overriding a generated record detaches it

Editing a generated PTR directly is refused, with an error naming the source record. A user
who wants a different PTR calls an explicit **detach** operation: the record loses
`managed_by`/`managed_kind` and becomes an ordinary authored record that the automation no
longer touches.

A "pin" flag, keeping the link but overriding the value, was rejected because it creates a
third record state that every consumer has to understand, in exchange for a warning nobody
reads.

**Consequence, accepted knowingly:** a detached PTR survives the deletion of the A record it
was originally derived from. It is an authored record at that point, and deleting authored
data as a side effect of an unrelated change is worse than leaving a stale record that the
consistency check will flag.
