# D5a — One delegation rule, and it is the strict one

At a delegation point only NS may live, and below it only A and AAAA glue (RFC 1034 §4.2.1).
Everything else there is referred to the child and never answered, so storing it means
holding data the server will not serve.

The rule lives in one function, `zone.ValidateUnderDelegation`, called both by the whole-zone
check and by the incremental write path. Before, only the first had it: creating a zone with
a record below a delegation was refused, and adding the same record a moment later was
accepted. The same end state was therefore accepted or refused depending on the order it was
reached in.

Strict rather than permissive, though BIND merely warns. Strictness is reversible and a store
full of records nobody can see is not, and this server's whole premise is that it does not
let a person build something that quietly does not work.

**Consequence, accepted knowingly:** the zonefile importer will meet real zones that carry
occluded records, and cannot simply refuse the file. It gets to decide (skip them and report
what it skipped, most likely) and that is a decision for the commit that builds it, made
deliberately rather than inherited from a gap here.
