/**
 * What an answer of the server is, in words (D49).
 */
import type { AnsweredRecord, Asked } from "#lib/api/index.ts";

/** headline names the kind of answer, the way the page leads with it. */
export const headline: Record<Asked["outcome"], string> = {
  answer: "Answered",
  alias: "An alias",
  nodata: "No data",
  nxdomain: "No such name",
  referral: "Referred",
  refused: "Refused",
  notImplemented: "Not answered",
  failed: "Failed",
};

/**
 * tone is how loudly to say it. An answer, an alias, no data and a referral
 * are the server doing what it should; the rest is what somebody came to find,
 * and so is an alias that leads to a name that does not exist.
 */
export function tone(a: Pick<Asked, "outcome" | "rcode">): "neutral" | "warn" | "crit" {
  switch (a.outcome) {
    case "answer":
    case "nodata":
    case "referral":
      return "neutral";
    case "alias":
      return a.rcode === "NOERROR" ? "neutral" : "warn";
    case "nxdomain":
    case "refused":
    case "notImplemented":
      return "warn";
    case "failed":
      return "crit";
  }
}

/** describe says what the answer holds, once the headline has said what kind it is. */
export function describe(a: Asked): string {
  const n = a.answer.length;
  const records = `${n} record${n === 1 ? "" : "s"}`;
  const what: Record<Asked["outcome"], string> = {
    answer: records,
    alias:
      `${records}, starting with the CNAME the question did not ask for` +
      (a.rcode === "NXDOMAIN" ? ", and the name it leads to does not exist" : ""),
    nodata: `${a.name} exists and has no ${a.type} record`,
    nxdomain: "",
    referral: "The name is delegated, and the name servers below answer for it",
    refused: "",
    notImplemented: "",
    failed: "",
  };
  return [what[a.outcome], a.explanation].filter(Boolean).join("; ");
}

/**
 * target is the name a record's data points at, where it points at one: what
 * a person exploring an answer asks about next.
 */
export function target(r: AnsweredRecord): string | undefined {
  const parts = r.data.trim().split(/\s+/);
  switch (r.type) {
    case "CNAME":
    case "DNAME":
    case "NS":
    case "PTR":
      return parts[0];
    case "MX":
      return parts[1];
    case "SRV":
      return parts[3];
  }
  return undefined;
}

/** asText is the answer as a zonefile writes it, a section to a block. */
export function asText(a: Asked): string {
  const line = (r: AnsweredRecord) => [r.name, r.ttl, r.class, r.type, r.data].join("\t");
  const out = [`; ${a.name} ${a.type}: ${a.rcode}${a.authoritative ? ", authoritative" : ""}`];
  for (const [title, rows] of [
    ["answer", a.answer],
    ["authority", a.authority],
    ["additional", a.additional],
  ] as const) {
    if (rows.length === 0) continue;
    out.push("", `; ${title}`, ...rows.map(line));
  }
  return out.join("\n");
}
