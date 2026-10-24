import { untrack } from "svelte";

import { goto } from "$app/navigation";
import { page } from "$app/state";

/**
 * The requests already taken out of an address, by the navigation that
 * brought them. Taking one out is a shallow navigation, which changes what the
 * address bar shows and leaves page.url as the navigation left it, so page.url
 * still carries the request afterwards. Remembering it here is what stops the
 * same request from being acted on again, and a new navigation is a new URL
 * object that starts with a clean slate.
 */
const taken = new WeakMap<object, Set<string>>();

/**
 * asked reports whether the address carries a one-time request, such as the
 * ?new in /zones?new, and takes it out of the address again. Left in, a reload
 * or the Back button would open the same dialog a second time.
 *
 * Called from an effect, it reads the address and so runs again when another
 * page, or the command palette, asks the same thing. It depends on nothing
 * else: page.state, which taking the request out changes, is read untracked.
 */
export function asked(name: string): boolean {
  const url = page.url;
  const already = taken.get(url) ?? new Set<string>();
  if (!url.searchParams.has(name) || already.has(name)) return false;
  already.add(name);
  taken.set(url, already);

  untrack(() => {
    const next = new URL(url.href);
    next.searchParams.delete(name);
    void goto(next, { state: page.state, shallow: true, replace: true });
  });
  return true;
}
