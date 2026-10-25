# D53 — Three kinds of page, the full width, and a dialog or a page by how much there is

- Decided: 2026-10-25

## Context

The interface grew a screen at a time, and each screen decided its own shape. Settings was a
column of panels capped at the width of a book and centred, the zone's settings capped its
fields at another width, a provider's page at a third, and the panel itself at a fourth.
The tables beside them ran edge to edge. On a large screen a page of four short settings had
to be scrolled through while half the screen stayed empty.

Making things had no rule either. A token, a key, a zone and a record were made in a dialog;
an identity provider on a page of its own, where editing it meant finding the page and
removing it meant scrolling to its foot. Asked to remove one, a person looked at the list
and found no way to.

## Decision

**A page is one of three kinds, and takes the width there is.**

- *A list* is a table from edge to edge under the command bar, with the actions that make
  something in the bar. A row says what it is about and carries what can be done to it, in
  plain sight rather than on hover alone.
- *Settings* are rows grouped into sections and, where there are enough sections to need it, a
  list beside them that jumps to each. A row is one decision: what it is and why on the left,
  the control on the right, and the two stack on a narrow screen. Sections are told apart by
  their heading and a rule, not by boxes, so a page of many settings reads evenly instead of as
  a wall of cards.
- *An overview* is sections in a grid, for the pages that report rather than ask: the
  overview itself, the cluster.

No page centres a column of fixed width. A row's explanation keeps a readable measure in its
column, and a control stops widening where an input is still easy to read.

**A thing is made and changed in a dialog when it fits in one, and on a page of its own when
it does not.** A dialog is for a thing of one part and a handful of fields, about six, seen
without scrolling: a token, a key, a zone, a record. A page is for a thing of several parts,
or one that is looked at as well as changed: a zone, an identity provider. Either way, the
same form makes it and changes it. A page that can try what is being made shows the attempt
in a column beside the form, which stays in view while the form scrolls.

**Removing a thing is reachable from its row and from its page.** A page puts its actions in
the command bar, the destructive one among them, rather than at the bottom of the page.
Removing asks once, in a dialog saying what goes with it.

## Consequences

A screen added later is one of the three kinds, built from the same pieces, rather than a
fourth shape. The pieces are `SettingsLayout`, `Section` and `Row` for settings, `Table`
for a list and `Dialog` for the questions; [design.md](../design.md) says how each is
used, and how the CLI is held to the same standard.

A row has the same shape on every settings page, so a person who has used one knows where to
look on the next.

## Where this stands

The settings page, a zone's settings and an identity provider's page are laid out in rows,
the provider's with its check beside the form, and the provider list carries its actions in
each row. The other lists already were what this describes, apart from their rows' actions,
which some show only on hover.
