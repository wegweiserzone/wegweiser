/**
 * How this server stands, for every page at once: whether it serves, whether
 * its cluster takes writes, and so whether a change made here can land.
 *
 * Every page used to find out for itself, and most never did: a cluster that
 * had lost its majority was visible on the Cluster page alone, and everywhere
 * else a change was offered, filled in, and then refused.
 */
import { api } from "$lib/api";
import type { ClusterStatus, Health } from "$lib/api";
import { verdict } from "$lib/quorum";
import type { Verdict } from "$lib/quorum";
import { session } from "$lib/session.svelte";

let health = $state<Health | null>(null);
/** cluster is null on a single server, and before the first answer. */
let cluster = $state<ClusterStatus | null>(null);

async function refresh() {
  try {
    health = await api.get("/healthz");
  } catch {
    health = null;
  }
  try {
    cluster = await api.get("/cluster");
  } catch {
    // A single server answers 404, which is a way to run rather than a fault.
    // Anything else leaves nothing to say about a cluster either.
    cluster = null;
  }
}

/** interval is how often the standing is asked again while the page is in view. */
const interval = 10_000;

export const standing = {
  get health(): Health | null {
    return health;
  },

  get cluster(): ClusterStatus | null {
    return cluster;
  },

  /** verdict is whether the cluster takes writes, or null on a single server. */
  get verdict(): Verdict | null {
    return cluster ? verdict(cluster) : null;
  },

  /** out is a member that has left its cluster, either way it got there. */
  get out(): boolean {
    return health?.current === false;
  },

  /**
   * writes says whether a change made in this session can land, and if not,
   * why: the reason is what a control that is withheld says instead.
   */
  get writes(): { allowed: boolean; reason: string } {
    if (!session.can("write")) {
      return { allowed: false, reason: "Changing this needs a token with the write scope." };
    }
    if (this.out) {
      return {
        allowed: false,
        reason: "This server has left its cluster: it refuses writes, and answers queries with what it held.",
      };
    }
    const v = this.verdict;
    if (v && v.tone === "crit") {
      return { allowed: false, reason: `The cluster takes no writes: ${v.detail}` };
    }
    return { allowed: true, reason: "" };
  },

  /**
   * stopped is a session that could change things and cannot now. It is told
   * once, across every page; a session that may only read is told where a
   * change would be instead.
   */
  get stopped(): boolean {
    return session.can("write") && !this.writes.allowed;
  },

  /** withheld names what stands where a change would be, by why it is withheld. */
  get withheld(): string {
    return session.can("write") ? "No writes" : "Read only";
  },

  /**
   * watch keeps the standing current while the page is open and in view, and
   * returns what stops it.
   */
  watch(): () => void {
    void refresh();
    const timer = setInterval(() => {
      if (!document.hidden) void refresh();
    }, interval);
    return () => clearInterval(timer);
  },

  /** refresh asks again now, after something that may have changed it. */
  refresh,
};
