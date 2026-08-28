/**
 * How the interface writes things down.
 */

/** ago says how long since a moment, in the coarsest unit that is still true. */
export function ago(when: string | undefined): string {
  if (!when) return "—";
  const then = new Date(when).getTime();
  if (Number.isNaN(then)) return "—";

  const seconds = Math.round((Date.now() - then) / 1000);
  if (seconds < 0) return "just now"; // a clock that disagrees is not worth a sentence
  if (seconds < 45) return "just now";
  if (seconds < 90) return "a minute ago";

  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} min ago`;

  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours} h ago`;

  const days = Math.round(hours / 24);
  if (days < 7) return `${days} d ago`;

  // Past a week the exact date is more use than a count of weeks.
  return new Date(then).toLocaleDateString(undefined, { day: "numeric", month: "short" });
}

/** exact is the full timestamp, for the title of something shown loosely. */
export function exact(when: string | undefined): string {
  if (!when) return "";
  const at = new Date(when);
  return Number.isNaN(at.getTime()) ? "" : at.toLocaleString();
}

/** count writes a number with the separators a person expects. */
export function count(n: number | undefined): string {
  return n === undefined ? "—" : n.toLocaleString("en").replace(/,/g, " ");
}

/**
 * relative shortens a name against the zone it sits in, the way a zonefile
 * does: www.example.com. inside example.com. is www, and the apex is @.
 */
export function relative(name: string, apex: string): string {
  if (name === apex) return "@";
  if (apex && name.endsWith("." + apex)) return name.slice(0, -(apex.length + 1));
  return name;
}
