# D14 — QTYPE=ANY is answered with one RRset

A query for ANY is answered with a single RRset from the name: the CNAME if there is one,
otherwise the lowest type number present. Not everything the name holds, and not the
synthetic HINFO of RFC 8482 §4.2.

RFC 8482 §4.1 permits this. The full contents of a name is an amplification lever, one small
question for one large answer, and an authoritative server cannot tell a reflection attack
from a curious operator. The other bounds in the query path exist for the same reason. The
synthetic HINFO was rejected separately, because it puts a record on the wire that is not in
the zone.

The cost is real: `dig ANY` is a common way to inspect a name and it stops working here.
The record editor, `weg record list` and the live query stream cover that, and none of them
is reachable by an attacker.

**Obligation.** The choice of RRset is deterministic, so an answer never depends on the
order records happened to be stored in. Two zones with the same records in a different order
answer ANY identically.
