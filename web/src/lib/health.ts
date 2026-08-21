/**
 * The one thing about a zone that looks fine and is not.
 *
 * A resolver that follows a delegation to a name server *inside* the zone it
 * delegates to has a circular dependency, and the only way out is an address
 * record served alongside it: glue. Without one this server answers
 * NXDOMAIN, authoritatively, for its own name server, and the delegation is
 * lame (RFC 1912 §2.8). Nothing else about the zone looks wrong.
 *
 * Kept in step with lameNameServers in internal/cli/zone_health.go, which is
 * the same rule for the other client. Two implementations because the rule is
 * a diagnosis rather than data, and neither client should have to wait for the
 * other. If a second rule ever joins this one, it belongs on the server with
 * an ADR of its own rather than in both clients twice.
 */

import { api } from "./api";
import type { Zone } from "./api";

/** LameNS is a name server this zone points at and has no address for. */
export interface LameNS {
  /** The name the NS record sits on: the apex, or a delegated child. */
  owner: string;
  /** The name server that has no address here. */
  target: string;
}

/** inside reports whether a name is at or below an apex. */
function inside(name: string, apex: string): boolean {
  return name === apex || name.endsWith("." + apex);
}

/** lameNameServers finds the NS targets inside this zone that nothing answers for. */
export async function lameNameServers(zone: Zone): Promise<LameNS[]> {
  const servers = await api.get("/zones/{zoneId}/records", {
    path: { zoneId: zone.id },
    query: { type: "NS", limit: 1000 },
  });

  // One lookup per distinct target, not per record: two name servers on one
  // name is one question.
  const asked = new Set<string>();
  const lame: LameNS[] = [];

  for (const ns of servers.items) {
    // A target outside the zone needs nothing from us: it is resolved the
    // ordinary way, which is what an off-site secondary is for.
    if (!inside(ns.data, zone.name) || asked.has(ns.data)) continue;
    asked.add(ns.data);

    const addressed = await Promise.all(
      (["A", "AAAA"] as const).map((type) =>
        api.get("/zones/{zoneId}/records", {
          path: { zoneId: zone.id },
          query: { name: ns.data, type, limit: 1 },
        }),
      ),
    );
    if (addressed.every((page) => page.items.length === 0)) {
      lame.push({ owner: ns.name, target: ns.data });
    }
  }
  return lame;
}
