/**
 * nearest is the candidate a typed name most likely meant: the closest one,
 * provided it is at most two edits away. Two catches a slipped or swapped
 * letter; further than that it is a different name rather than a typo.
 */
export function nearest(name: string, candidates: string[]): string | undefined {
  let best: string | undefined;
  let closest = 3;
  for (const candidate of candidates) {
    const d = distance(name.toLowerCase(), candidate.toLowerCase());
    if (d < closest) [best, closest] = [candidate, d];
  }
  return best;
}

/** distance is how many letters have to be added, removed or changed. */
function distance(a: string, b: string): number {
  let previous = Array.from({ length: b.length + 1 }, (_, j) => j);
  for (let i = 1; i <= a.length; i++) {
    const row = [i];
    for (let j = 1; j <= b.length; j++) {
      const change = a[i - 1] === b[j - 1] ? 0 : 1;
      row[j] = Math.min(previous[j]! + 1, row[j - 1]! + 1, previous[j - 1]! + change);
    }
    previous = row;
  }
  return previous[b.length]!;
}
