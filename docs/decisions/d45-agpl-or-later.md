# D45 — AGPL-3.0, or any later version

- Decided: 2026-10-04
- Amends: [D13](d13-dco-not-cla.md)

The licence is the GNU Affero General Public License, version 3 or any later version. Its
SPDX identifier is `AGPL-3.0-or-later`, and that is the identifier wherever one is written.

D13 chose AGPLv3 and said nothing about later versions, so the repository answered the
question three ways. The OpenAPI document and the container image's label said `or-later`,
`web/package.json` said `only`, and the README and the conventions said "AGPLv3", which fits
either. A reader should not have to work out which licence applies from which file they
opened first.

**Why "or later".** Contributions arrive under the DCO, with no CLA and no copyright
assignment (D13), so nobody holds the rights to the whole of the code. Under `only`, moving
to a future AGPL would take the consent of every contributor, found and asked one at a time,
and that gets less possible with every contributor added. `or later` is the one form in which
the code can follow its licence forward without it. It is also what the licence's own "How
to Apply" appendix prints.

**The cost, acknowledged.** "Any later version" includes a version nobody has read yet: its
publisher decides terms the code becomes available under, and a recipient may choose them.
The Linux kernel says `only` for exactly that reason. Here the trade runs the other way. A
later version that weakened the network clause would contradict the reason that licence
exists, while a project that cannot leave version 3 without every contributor's signature is
stuck the first time one of them cannot be reached.

**Obligation.** Package metadata, the API document and the image label carry
`AGPL-3.0-or-later`. Prose may say "AGPLv3 or later". `LICENSE` stays the unmodified text of
version 3.
