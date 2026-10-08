/**
 * Whether the cluster takes writes, read from the counts the server gives, and
 * what taking a member out would leave of that. The command line says the same
 * things from the same counts (internal/cli/cluster.go).
 */
import type { ClusterStatus } from "$lib/api";

type Member = ClusterStatus["members"][number];

export type Tone = "ok" | "warn" | "crit";

export interface Verdict {
  tone: Tone;
  /** The answer, in a few words. */
  headline: string;
  /** The counts it rests on, as a sentence. */
  detail: string;
}

/** answers is whether a member answered and still takes part. */
export const answers = (m: Member) => Boolean(m.progress && !m.progress.removed && !m.progress.behind);

/** votes is whether a member counts towards the majority. */
export const votes = (m: Member) => m.role !== "nonvoter";

/** verdict is what the counts amount to, or null where the server gave none. */
export function verdict(status: ClusterStatus): Verdict | null {
  const q = status.quorum;
  if (!q) return null;
  const led = status.members.some((m) => m.leader);
  const spare = q.answered - q.needed;
  const counts = `${q.answered} of ${q.voters} voters answered, and ${q.needed} are needed`;

  if (spare < 0) {
    return {
      tone: "crit",
      headline: "Takes no writes",
      detail: `${counts}. Every member goes on answering queries; writes wait for a majority to be back.`,
    };
  }
  if (!led) {
    return { tone: "warn", headline: "Waiting for a leader", detail: `${counts}. Writes resume once one is elected.` };
  }
  if (spare === 0) {
    return {
      tone: "warn",
      headline: "One failure from stopping",
      detail: `${counts}, so the next voter lost stops writes.`,
    };
  }
  return {
    tone: "ok",
    headline: "Takes writes",
    detail: `${counts}, so ${spare === 1 ? "one voter" : `${spare} voters`} can be lost.`,
  };
}

/**
 * afterRemoving says what taking a member out leaves of the majority, or null
 * where there is nothing worth saying: no counts, or no majority to start
 * with, in which case the removal is refused anyway.
 */
export function afterRemoving(status: ClusterStatus, id: string): { tone: Tone; text: string } | null {
  const q = status.quorum;
  const target = status.members.find((m) => m.id === id);
  if (!q || !target || q.answered < q.needed) return null;
  if (!votes(target)) return { tone: "ok", text: `${id} does not vote, so writes go on as they are.` };

  const voters = q.voters - 1;
  const answered = q.answered - (answers(target) ? 1 : 0);
  const needed = Math.floor(voters / 2) + 1;
  const left = `Afterwards ${voters} voters remain, ${answered} of them answered here, and ${needed} are needed`;
  if (answered < needed) return { tone: "crit", text: `${left}: the cluster would take no writes.` };
  if (answered === needed) return { tone: "warn", text: `${left}: writes go on, and the next voter lost stops them.` };
  return { tone: "ok", text: `${left}: writes go on.` };
}
