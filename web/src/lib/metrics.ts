/**
 * What this server has been doing, read from its own metrics.
 */

import { ApiError, NetworkError, basePath } from "./api";
import type { Problem } from "./api";

/** Sample is one line of the exposition format. */
export interface Sample {
  name: string;
  labels: Record<string, string>;
  value: number;
}

/** Reading is a name and a number, for the small bar charts. */
export interface Reading {
  label: string;
  count: number;
}

/** Readings is everything the overview shows. */
export interface Readings {
  /** Every query answered since this process started. */
  answered: number;
  /** Answers by response code, commonest first. */
  byRcode: Reading[];
  /** Questions by type, commonest first. */
  byType: Reading[];
  /** Queries by transport. */
  udp: number;
  tcp: number;
  /** Queries that got no response at all, two messages have no safe reply. */
  dropped: number;
  /** Responses cut to fit the transport and marked TC (RFC 1035 §4.1.1). */
  truncated: number;
  /** The share answered inside a millisecond, which is what D12 targets. */
  withinTarget: number;
  /** The latency buckets, as upper bounds in seconds and how many fell under. */
  latency: { bound: number; count: number }[];
  /** When this process started, for the uptime. */
  startedAt: Date | null;
  /** When the snapshot being answered from was published. */
  snapshotAt: Date | null;
  zones: number;
  records: number;
}

/** read fetches the metrics and turns them into what the overview shows. */
export async function read(signal?: AbortSignal): Promise<Readings> {
  let response: Response;
  try {
    response = await fetch(`${basePath}/metrics`, {
      headers: { Accept: "text/plain" },
      credentials: "same-origin",
      signal,
    });
  } catch (cause) {
    throw new NetworkError(cause);
  }

  if (!response.ok) {
    let problem: Problem | undefined;
    try {
      problem = (await response.json()) as Problem;
    } catch {
      // Not one of ours, which the error below says plainly enough.
    }
    throw new ApiError(
      problem?.title
        ? problem
        : {
            type: "/problems/unknown",
            title: `The server answered ${response.status}`,
            status: response.status,
          },
    );
  }

  return summarise(parse(await response.text()));
}

/**
 * parse reads the exposition format.
 */
export function parse(text: string): Sample[] {
  const samples: Sample[] = [];

  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (line === "" || line.startsWith("#")) continue;

    const brace = line.indexOf("{");
    let name: string;
    let labels: Record<string, string> = {};
    let rest: string;

    if (brace < 0) {
      const space = line.indexOf(" ");
      if (space < 0) continue;
      name = line.slice(0, space);
      rest = line.slice(space + 1);
    } else {
      name = line.slice(0, brace);
      const close = closingBrace(line, brace);
      if (close < 0) continue;
      labels = readLabels(line.slice(brace + 1, close));
      rest = line.slice(close + 1);
    }

    // A sample may carry a timestamp after the value, and an exemplar after
    // that. The value is the first field either way.
    const value = Number(rest.trim().split(/\s+/)[0]);
    if (!Number.isNaN(value)) samples.push({ name, labels, value });
  }

  return samples;
}

/** closingBrace finds the label set's end, respecting quoted values. */
function closingBrace(line: string, from: number): number {
  let quoted = false;
  for (let i = from + 1; i < line.length; i++) {
    const c = line[i];
    if (quoted) {
      if (c === "\\") i++;
      else if (c === '"') quoted = false;
      continue;
    }
    if (c === '"') quoted = true;
    else if (c === "}") return i;
  }
  return -1;
}

/** readLabels reads `a="1",b="2"` into an object. */
function readLabels(text: string): Record<string, string> {
  const labels: Record<string, string> = {};
  let i = 0;

  while (i < text.length) {
    const equals = text.indexOf("=", i);
    if (equals < 0) break;
    const key = text.slice(i, equals).trim();

    let j = text.indexOf('"', equals) + 1;
    if (j === 0) break;
    let value = "";
    for (; j < text.length; j++) {
      const c = text[j];
      if (c === "\\") {
        const next = text[j + 1];
        value += next === "n" ? "\n" : (next ?? "");
        j++;
        continue;
      }
      if (c === '"') break;
      value += c;
    }
    labels[key] = value;
    i = text.indexOf(",", j) + 1;
    if (i === 0) break;
  }

  return labels;
}

/** summarise turns the samples into the handful of numbers worth showing. */
function summarise(samples: Sample[]): Readings {
  const one = (name: string): number | undefined =>
    samples.find((s) => s.name === name)?.value;
  const all = (name: string): Sample[] => samples.filter((s) => s.name === name);

  const queries = all("weg_dns_queries_total");
  const answered = queries.reduce((sum, s) => sum + s.value, 0);

  const total = (label: string) => {
    const counts = new Map<string, number>();
    for (const sample of queries) {
      const key = sample.labels[label] ?? "?";
      counts.set(key, (counts.get(key) ?? 0) + sample.value);
    }
    return [...counts]
      .map(([name, count]) => ({ label: name, count }))
      .filter((r) => r.count > 0)
      .sort((a, b) => b.count - a.count);
  };

  const byTransport = new Map(total("transport").map((r) => [r.label, r.count]));

  // The histogram is cumulative: each bucket holds everything at or under its
  // bound, so the counts have to be differenced to be read as a distribution.
  const cumulative = new Map<number, number>();
  for (const sample of all("weg_dns_query_duration_seconds_bucket")) {
    const bound = Number(sample.labels.le);
    if (Number.isNaN(bound)) continue;
    cumulative.set(bound, (cumulative.get(bound) ?? 0) + sample.value);
  }
  const bounds = [...cumulative.keys()].sort((a, b) => a - b);

  let previous = 0;
  const latency = bounds.map((bound) => {
    const under = cumulative.get(bound) ?? 0;
    const count = Math.max(0, under - previous);
    previous = under;
    return { bound, count };
  });

  const counted = all("weg_dns_query_duration_seconds_count").reduce(
    (sum, s) => sum + s.value,
    0,
  );
  const withinMillisecond = cumulative.get(0.001) ?? 0;

  const seconds = (name: string) => {
    const at = one(name);
    return at ? new Date(at * 1000) : null;
  };

  return {
    answered,
    byRcode: total("rcode"),
    byType: total("type"),
    udp: byTransport.get("udp") ?? 0,
    tcp: byTransport.get("tcp") ?? 0,
    dropped: all("weg_dns_queries_dropped_total").reduce((sum, s) => sum + s.value, 0),
    truncated: all("weg_dns_responses_truncated_total").reduce((sum, s) => sum + s.value, 0),
    withinTarget: counted === 0 ? 1 : withinMillisecond / counted,
    latency,
    startedAt: seconds("process_start_time_seconds"),
    snapshotAt: seconds("weg_snapshot_published_timestamp_seconds"),
    zones: one("weg_snapshot_zones") ?? 0,
    records: one("weg_snapshot_records") ?? 0,
  };
}
