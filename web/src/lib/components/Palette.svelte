<script lang="ts">
  /**
   * Ctrl+K: everything this interface can do, from the keyboard.
   */
  import { goto } from "$app/navigation";
  import { api } from "$lib/api";
  import type { Zone } from "$lib/api";
  import { session } from "$lib/session.svelte";
  import { theme } from "$lib/theme.svelte";

  interface Command {
    group: string;
    label: string;
    hint?: string;
    run: () => void | Promise<void>;
  }

  let open = $state(false);
  let typed = $state("");
  let cursor = $state(0);
  let zones = $state<Zone[]>([]);
  let field = $state<HTMLInputElement | null>(null);

  /** Whether a `g` is waiting for the letter that says where to go. */
  let pending = false;
  let forget: ReturnType<typeof setTimeout> | undefined;

  /**
   * Where `g` goes. The palette lists these with their key, so the list is
   * also how anybody finds out the keys exist, which means the two have to
   * come from one place, or the hint on the screen becomes a promise the
   * keyboard does not keep.
   */
  const places = [
    { key: "o", label: "Overview", href: "/" },
    { key: "z", label: "Zones", href: "/zones" },
    { key: "s", label: "Query stream", href: "/stream" },
    { key: "h", label: "History", href: "/history" },
    { key: "t", label: "Tokens", href: "/tokens" },
  ];

  const sections: Command[] = places.map((place) => ({
    group: "Go",
    label: place.label,
    hint: `g ${place.key}`,
    run: () => goto(place.href),
  }));

  const commands = $derived.by(() => {
    const all: Command[] = [...sections];

    for (const zone of zones) {
      all.push({
        group: "Zones",
        label: zone.name,
        hint: zone.kind,
        run: () => goto(`/zones/${encodeURIComponent(zone.name)}`),
      });
    }

    all.push({
      group: "Interface",
      label: `Switch to the ${theme.resolved === "dark" ? "light" : "dark"} theme`,
      run: () => theme.toggle(),
    });
    all.push({ group: "Interface", label: "Sign out", run: () => session.close() });
    return all;
  });

  const matches = $derived.by(() => {
    const needle = typed.trim().toLowerCase();
    if (!needle) return commands;
    return commands.filter(
      (c) => c.label.toLowerCase().includes(needle) || c.group.toLowerCase().includes(needle),
    );
  });

  /** The groups, in the order their first command appears. */
  const grouped = $derived.by(() => {
    const out: { group: string; items: { command: Command; index: number }[] }[] = [];
    matches.forEach((command, index) => {
      const last = out.at(-1);
      if (last?.group === command.group) last.items.push({ command, index });
      else out.push({ group: command.group, items: [{ command, index }] });
    });
    return out;
  });

  async function show() {
    open = true;
    typed = "";
    cursor = 0;
    // Fetched when it is opened rather than kept in step: a palette that is
    // one keystroke away is also one keystroke away from being reopened, and
    // a stale zone list is the one thing that would make it untrustworthy.
    try {
      zones = (await api.get("/zones", { query: { limit: 1000 } })).items;
    } catch {
      zones = [];
    }
  }

  function hide() {
    open = false;
  }

  async function run(command: Command | undefined) {
    if (!command) return;
    hide();
    await command.run();
  }

  function onKeydown(event: KeyboardEvent) {
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "k") {
      event.preventDefault();
      if (open) hide();
      else void show();
      return;
    }
    if (event.key === "Escape" && open) {
      event.preventDefault();
      hide();
      return;
    }

    if (open) {
      if (event.key === "ArrowDown") {
        event.preventDefault();
        cursor = Math.min(cursor + 1, matches.length - 1);
      }
      if (event.key === "ArrowUp") {
        event.preventDefault();
        cursor = Math.max(cursor - 1, 0);
      }
      if (event.key === "Enter") {
        event.preventDefault();
        void run(matches[cursor]);
      }
      return;
    }

    const target = event.target;
    const typing =
      target instanceof HTMLElement &&
      (target.tagName === "INPUT" || target.tagName === "TEXTAREA" || target.isContentEditable);
    if (typing) {
      pending = false;
      return;
    }

    // g, then a letter. The window is short on purpose: a `g` left over from
    // five minutes ago turning the next keystroke into a navigation is worse
    // than having to press it again.
    if (pending) {
      pending = false;
      const place = places.find((p) => p.key === event.key);
      if (place) {
        event.preventDefault();
        void goto(place.href);
        return;
      }
    }
    if (event.key === "g") {
      pending = true;
      clearTimeout(forget);
      forget = setTimeout(() => (pending = false), 1200);
      return;
    }

    // One key focuses whatever this screen filters by.
    if (event.key === "/") {
      const search = document.querySelector<HTMLInputElement>(
        'input[aria-label^="Search"], input[aria-label^="Filter"]',
      );
      if (search) {
        event.preventDefault();
        search.focus();
      }
    }
  }
</script>

<svelte:window onkeydown={onKeydown} />

{#if open}
  <!-- The backdrop closes it, which is what everybody expects of one. -->
  <div
    class="fixed inset-0 z-100 grid place-items-start justify-center bg-sunken/70 pt-[13vh]
           backdrop-blur-[3px]"
    role="presentation"
    onclick={(e) => e.target === e.currentTarget && hide()}
  >
    <div
      class="w-[min(34rem,calc(100vw-2rem))] overflow-hidden rounded-md border border-line
             bg-surface shadow-[var(--shadow-lift)]"
      role="dialog"
      aria-modal="true"
      aria-label="Commands"
    >
      <div class="flex h-12 items-center gap-3 border-b border-line px-4">
        <svg
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          class="size-4 shrink-0 text-signal"
          aria-hidden="true"
        >
          <path d="m4 8 4 4-4 4M11 16h9" stroke-linecap="round" stroke-linejoin="round" />
        </svg>
        <!-- svelte-ignore a11y_autofocus -->
        <input
          bind:this={field}
          bind:value={typed}
          oninput={() => (cursor = 0)}
          autofocus
          autocomplete="off"
          spellcheck="false"
          placeholder="Go somewhere, or type a zone name…"
          aria-label="Command"
          class="num min-w-0 flex-1 bg-transparent text-[14px] outline-none placeholder:text-ink-faint"
        />
      </div>

      <div class="max-h-[21rem] overflow-y-auto p-1.5">
        {#each grouped as section (section.group)}
          <p class="sign px-2 pt-2 pb-1 text-[10px] text-ink-faint">{section.group}</p>
          {#each section.items as { command, index } (command.group + command.label)}
            <button
              type="button"
              onmouseenter={() => (cursor = index)}
              onclick={() => run(command)}
              class="flex w-full cursor-pointer items-center gap-2.5 rounded-sm px-2 py-1.5
                     text-left text-[13px] {index === cursor
                ? 'bg-signal-lo text-signal'
                : 'text-ink'}"
            >
              <span class="num truncate">{command.label}</span>
              {#if command.hint}
                <span class="num ml-auto text-[11px] text-ink-faint">{command.hint}</span>
              {/if}
            </button>
          {/each}
        {:else}
          <p class="px-3 py-8 text-center text-[13px] text-ink-mute">
            Nothing matches <span class="num text-ink">{typed}</span>.
          </p>
        {/each}
      </div>

      <div class="flex gap-4 border-t border-line bg-raised px-4 py-2 text-[11px] text-ink-faint">
        <span class="flex items-center gap-1.5"><kbd class="num">↑↓</kbd> move</span>
        <span class="flex items-center gap-1.5"><kbd class="num">↵</kbd> go</span>
        <span class="flex items-center gap-1.5"><kbd class="num">esc</kbd> close</span>
        <span class="flex items-center gap-1.5"><kbd class="num">g</kbd> then a letter</span>
        <span class="ml-auto flex items-center gap-1.5"><kbd class="num">/</kbd> filter this screen</span>
      </div>
    </div>
  </div>
{/if}
