# D8 — Journal retention is unlimited by default

Full history is kept unless an operator opts into a retention policy, configured per zone as
"keep N commits" or "keep D days". Truncation always writes a checkpoint first, so rollback
to a truncated serial still works by restoring the checkpoint and replaying forward.

Time travel is a headline feature; a default that quietly discards history would undermine
it. Operators who cannot afford the growth can bound it explicitly, and the checkpoint
mechanism means bounding it does not cost them the ability to roll back.

**Where this stands: only the default is built.** Everything is kept, because keeping it all is
what happens when nothing truncates. The opt-in policy, the checkpoints and the metric below
are not written, and the scope fence does not list them. They are the work this entry
describes, not a description of the code.

**Obligation, once it is built.** `weg zone history` and the GUI show when history is
truncated, so a missing older state reads as the policy it is. Journal size per zone is a
Prometheus metric.
