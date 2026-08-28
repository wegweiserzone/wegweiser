# D9 — The query stream filters server-side, then samples, and says so

1. Filters are applied **server-side**, before the ring buffer, so a narrow filter stays
   complete even at high query rates.
2. Sampling kicks in only when the *filtered* rate exceeds the configured cap.
3. The active sampling ratio is always displayed in the UI.

The ring buffer drops events when full rather than applying back-pressure. **Confirmed
trade:** under extreme load the showcase feature loses events rather than slowing DNS down.
The data plane never waits on observability.

A stream that drops events while looking complete misleads whoever is reading it, so the
ratio is shown in the interface rather than buried in a debug field.
