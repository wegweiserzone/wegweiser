# D1 — Module path

`github.com/wegweiserzone/wegweiser`, and the binary it builds is `weg`.

A module path is worth a record because of where it ends up. It is in every import line in
this tree, and in the import line of anybody who ever depends on a package here, so changing
it later rewrites every file and breaks every dependent at once.

The packaging names that go with it, the unit, the configuration directory and the image,
are in the technology table of [conventions.md](../conventions.md) alongside the rest of the
delivery decisions.
