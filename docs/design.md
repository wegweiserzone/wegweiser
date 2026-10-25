# Design

How the interface and the CLI are built, in practice. The bar both are held to is in
[conventions.md](conventions.md), under *GUI design bar* and *CLI design bar*, and the
layout of a page is settled in [D53](decisions/d53-how-the-interface-is-laid-out.md). This
is the part that grows with the code: which piece to reach for, and how it is used.

## The interface

### The shell

Every page sits in the same frame. The rail on the left lists the sections, the command bar
across the top names the page and holds its actions, and the pane under the bar is what
scrolls. On a narrow screen the rail waits behind a button. The rail (`Rail.svelte`) and the
command palette (`Ctrl K`, and `g` followed by a section's key, in `Palette.svelte`) list the
same sections in the same order, so a section added to one is added to the other.

### Three kinds of page

| Kind | For | Built from |
| --- | --- | --- |
| List | Things there are many of: zones, tokens, keys, providers | `Bar`, then `Table` as the pane itself, with `Empty` for nothing yet |
| Settings | Decisions, one per row | `Bar`, then `SettingsLayout` holding `Section`s of `Row`s |
| Overview | Reports: the overview, the cluster | `Bar`, then a scrolling pane of sections in a grid |

A list's table is the pane itself, so that its header sticks to the top of what scrolls. A
row's first cell names the thing and links to it where it has a page of its own; its last cell
carries what can be done to it, visible without hovering.

`SettingsLayout` is the pane: it scrolls, and given `sections` it lists them beside the page so
that a click jumps to one; a page of one or two sections passes none. A `Section` is a heading,
a rule and its rows, with an optional `blurb` about the whole group. A `Row` is one decision:
`title` and `about` on the left, the control on the right. Several fields in one row share a
grid that fills the control's column, `repeat(auto-fill, minmax(…, 1fr))`. Each row is a
`section` element with its title as heading, which is what a test finds it by.

### Making and changing

A thing of one part and a handful of fields is made and changed in a `Dialog`; a thing of
several parts, or one that is looked at as well as changed, on a page of its own (D53). A
dialog opens from a button in the command bar, and from the palette through the address:
`/tokens?new` opens it, and `asked("new")` takes the request back out of the address so that
a reload does not open it again. Something made on its own page lands on that page, which
says so once (`?added`).

The command bar of a thing's page holds its actions in the order they are reached for: back,
the ones that try or look, the destructive one, and the primary one last. Removing asks once,
in a dialog that says what goes with the thing.

### Forms

`Field` is a labelled input with a `hint` under it, which a `problem` replaces when the value
is wrong. A secret is typed and never shown again: the field of a saved one is empty, with a
placeholder saying the saved one is kept. A transfer key's secret is the exception, read back
on purpose because the server has to keep it ([D28](decisions/d28-tsig.md)).

Where saving can be tried first, it is. An identity provider's page keeps its check in a
column beside the form that stays in view: it tries the fields as they stand, saved or not,
says when they have changed since, and lists the groups of somebody looked up, each of which a
click gives a scope.

### Saying things

`Notice` is a sentence that needs reading: `crit` for something that failed, `warn` for
something that needs attention, `signal` for news, such as a change that was saved. `Chip` is
a state, read before the words around it. An error shown is the server's own `detail`, which
says what broke and usually what to do. An empty state says what the thing
is for and offers the way to make one.

Text is plain sentences in English, written to the person reading it. A label is a noun, a
button a verb.

### Colour, type and motion

The tokens are defined once, in `web/src/app.css`, and a component that writes a colour
value is a bug. One accent, spent on five things that file names; every other colour is a
state. Barlow for prose, Barlow Condensed in capitals (`sign`) for labels and headings,
JetBrains Mono with tabular figures (`num`) for anything technical. State changes take 160
ms on one easing curve, and a viewer who asks for reduced motion gets none.

### Tests

Each page has a Playwright file in `web/tests/` that drives it as a person would, through
roles and labels. An accessible name is therefore part of a page's contract: renaming a
button is a change a test notices.

## The CLI
 ### Shape

`weg NOUN VERB`, with the verbs the same across nouns (`list`, `show`, `add` or `create`,
`set`, `remove` or `revoke`), short aliases beside them (`ls`, `rm`), and the help grouping
commands by what somebody came to do. Every command that talks to a server takes `--server` and
`--token`, and every command takes `--output text|json|yaml`.

### Output

What a command found goes to standard output through the printer, as text for a person or as
JSON or YAML for a script; prose, hints and progress go to standard error, so that `$(weg …)`
captures the answer and nothing else. Colour appears only on a terminal, and never with
`NO_COLOR` set.

### Errors

An error is the server's sentence. A name that is not one suggests the one probably meant. A
mistake on the command line exits 2, a failure 1, an interrupt 130.

### Asking

A command that destroys asks first and takes `--yes` to skip the question. With no terminal to
ask on, it refuses and names `--yes`.

### Secrets

A secret other than the API token is read from a file, or from standard input with `-`, so that
it stays out of the shell's history and the process table. The token belongs in `$WEG_TOKEN`,
or in the session `weg login` keeps ([D52](decisions/d52-weg-login-holds-a-session.md)).

### Checks

A command that checks reports what it found and exits 0 either way, because the findings are
the answer; a script reads the verdict from `--output json`. A change that can be tried first
takes `--dry-run`, which tries it and saves nothing.

### Tests

Commands live in `internal/cli` rather than in `package main`, and their tests run them through
the same entry point as the binary, against a server started in the test.
