import { replaceState } from "$app/navigation";
import { page } from "$app/state";

/**
 * asked reports whether the address carries a one-time request, such as the
 * ?new in /zones?new, and takes it out of the address again. Left in, a reload
 * or the Back button would open the same dialog a second time.
 *
 * Called from an effect, it reads the address and so runs again when another
 * page, or the command palette, asks the same thing.
 */
export function asked(name: string): boolean {
  if (!page.url.searchParams.has(name)) return false;
  const url = new URL(page.url);
  url.searchParams.delete(name);
  replaceState(url, page.state);
  return true;
}
