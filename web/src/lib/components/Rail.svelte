<script lang="ts">
  /**
   * The left rail: where you are, where you can go, and who you are.
   */
  import { page } from "$app/state";
  import { session } from "$lib/session.svelte";
  import { theme } from "$lib/theme.svelte";
  import type { Health } from "$lib/api";
  import Mark from "./Mark.svelte";

  let { health }: { health: Health | null } = $props();

  type Item = { href: string; label: string; icon: string };

  // Paths, not components: the icons are one line each and a file per icon
  // would be five files that say less than this does.
  const items: Item[] = [
    {
      href: "/",
      label: "Overview",
      icon: "M4 13h6V4H4v9Zm10 7h6v-9h-6v9ZM4 20h6v-4H4v4ZM14 8h6V4h-6v4Z",
    },
    {
      href: "/zones",
      label: "Zones",
      icon: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18M3 12h18M12 3a15 15 0 0 1 0 18 15 15 0 0 1 0-18",
    },

    {
      href: "/stream",
      label: "Query stream",
      icon: "M2 12h4l3-7 4 15 3-8h6",
    },
    {
      href: "/history",
      label: "History",
      icon: "M3 12a9 9 0 1 0 3-6.7M3 4v5h5M12 8v4l3 2",
    },
    {
      href: "/tokens",
      label: "Tokens",
      icon: "M8 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8M12 12h9M18 12v4M15.5 12v3",
    },
    {
      href: "/secondaries",
      label: "Secondaries",
      icon: "M4 5h7v5H4zM13 14h7v5h-7M11 7.5h6a2 2 0 0 1 2 2v2M8 10v2a2 2 0 0 0 2 2h3",
    },
    {
      href: "/keys",
      label: "Keys",
      icon: "M15.5 8.5a3.5 3.5 0 1 0-3.2 3.5L4 20v0h3v-2h2v-2h2l1.3-1.3a3.5 3.5 0 0 0 3.2-6.2M16 7.5h.01",
    },
    {
      href: "/settings",
      label: "Settings",
      icon: "M12 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6M12 2v3M12 19v3M4.2 4.2l2.2 2.2M17.6 17.6l2.2 2.2M2 12h3M19 12h3M4.2 19.8l2.2-2.2M17.6 6.4l2.2-2.2",
    },
  ];

  const current = $derived(page.url.pathname);

  /**
   * A section stays marked while you are inside it: /zones/example.com. is
   * still Zones, and a rail that forgets where you are on the second click is
   * a rail that tells you nothing.
   */
  function inside(href: string): boolean {
    if (href === "/") return current === "/";
    return current === href || current.startsWith(href + "/");
  }
</script>

<aside class="sticky top-0 flex h-screen flex-col border-r border-line bg-surface">
  <div class="flex items-center gap-2.5 px-4 pt-4.5 pb-4">
    <Mark class="size-5.5 shrink-0 text-signal" />
    <div class="min-w-0">
      <p class="font-cond text-[19px] leading-none font-bold tracking-[0.13em] uppercase">
        Wegweiser
      </p>
      <p class="num mt-0.5 truncate text-[10px] text-ink-faint">
        {health?.version ?? "not serving"}
      </p>
    </div>
  </div>

  <nav class="flex flex-col gap-px px-2" aria-label="Sections">
    {#each items as item (item.href)}
      <a
        href={item.href}
        aria-current={inside(item.href) ? "page" : undefined}
        class="group relative flex items-center gap-2.5 rounded-sm px-2.5 py-1.5 text-ink-mute
               transition-colors hover:bg-raised hover:text-ink
               aria-[current=page]:bg-raised aria-[current=page]:text-ink"
      >
        <span
          class="absolute -left-2 h-[18px] w-[3px] rounded-r-sm bg-signal opacity-0
                 transition-opacity group-aria-[current=page]:opacity-100"
        ></span>
        <svg
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.8"
          class="size-4 shrink-0 opacity-85"
          aria-hidden="true"
        >
          <path d={item.icon} />
        </svg>
        {item.label}
      </a>
    {/each}
  </nav>

  <div class="mt-auto flex flex-col gap-3 border-t border-line p-3">
    <div class="flex items-center gap-2">
      <span
        class="grid size-6 shrink-0 place-items-center rounded-xs bg-signal-lo font-cond
               text-[12px] font-bold tracking-[0.06em] text-signal uppercase"
      >
        {(session.who?.name ?? "?").slice(0, 2)}
      </span>
      <div class="min-w-0">
        <p class="truncate text-xs leading-tight">{session.who?.name ?? "unknown"}</p>
        <p class="sign text-[10px] text-ink-faint">
          {session.who?.scopes?.at(-1) ?? "—"}
        </p>
      </div>

      <button
        type="button"
        onclick={() => theme.toggle()}
        title="Switch to the {theme.resolved === 'dark' ? 'light' : 'dark'} theme"
        aria-label="Switch theme"
        class="ml-auto grid size-7 cursor-pointer place-items-center rounded-sm text-ink-faint
               transition-colors hover:bg-raised hover:text-ink"
      >
        {#if theme.resolved === "dark"}
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" class="size-4">
            <circle cx="12" cy="12" r="4.5" />
            <path
              d="M12 2v2.5M12 19.5V22M2 12h2.5M19.5 12H22M4.9 4.9l1.8 1.8M17.3 17.3l1.8 1.8M19.1 4.9l-1.8 1.8M6.7 17.3l-1.8 1.8"
              stroke-linecap="round"
            />
          </svg>
        {:else}
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" class="size-4">
            <path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5Z" stroke-linejoin="round" />
          </svg>
        {/if}
      </button>

      <button
        type="button"
        onclick={() => session.close()}
        title="Sign out"
        aria-label="Sign out"
        class="grid size-7 cursor-pointer place-items-center rounded-sm text-ink-faint
               transition-colors hover:bg-crit-lo hover:text-crit"
      >
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" class="size-4">
          <path d="M15 4h3a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2h-3M10 16l-4-4 4-4M6 12h11" stroke-linecap="round" stroke-linejoin="round" />
        </svg>
      </button>
    </div>
  </div>
</aside>
