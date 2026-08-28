# D6 — Reverse zones are offered, never auto-created

Adding an address with no covering reverse zone does not silently skip PTR creation and does
not create the zone. The API returns a structured hint naming the zone that would be needed;
the GUI renders it as a one-click action, the CLI as a suggestion line.

Creating a zone is an assertion of authority over a namespace. Doing it as a side effect of
adding a record would be a surprise, and for public address space it would be wrong.

**Obligation.** The hint travels in the response body as structured data, not as prose in an
error string. Both clients render it; neither parses English.
