# D7 — RFC 2317 CNAMEs are generated when we hold the parent

When Wegweiser is authoritative for both a `/24` reverse zone and a classless child zone
under it, it generates the parent-side CNAMEs
(`10.2.0.192.in-addr.arpa. CNAME 10.0/25.2.0.192.in-addr.arpa.`) as managed records with
`managed_kind = 'rfc2317-cname'`.

This is the half of RFC 2317 that is tedious and error-prone by hand, and it is exactly the
kind of thing differentiator 1 promises to take care of. Only when we hold the parent —
records in someone else's zone are not ours to write.
