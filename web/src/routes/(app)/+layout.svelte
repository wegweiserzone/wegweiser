<script lang="ts">
  /**
   * Everything the interface shows once there is somebody to show it to.
   */
  import { page } from "$app/state";
  import { session } from "$lib/session.svelte";
  import { standing } from "$lib/standing.svelte";
  import Brand from "$lib/components/Brand.svelte";
  import Button from "$lib/components/Button.svelte";
  import Mark from "$lib/components/Mark.svelte";
  import Palette from "$lib/components/Palette.svelte";
  import Rail from "$lib/components/Rail.svelte";
  import SignIn from "$lib/components/SignIn.svelte";

  let { children } = $props();

  session.check();

  $effect(() => {
    if (session.status === "authenticated") return standing.watch();
  });

  // The Cluster page says the same thing at its top, and better.
  const stopped = $derived(standing.stopped && page.url.pathname !== "/cluster");

  /** menu is the rail opened over a narrow screen; going anywhere closes it. */
  let menu = $state(false);
  $effect(() => {
    void page.url.pathname;
    menu = false;
  });
</script>

<svelte:window onkeydown={(e) => e.key === "Escape" && (menu = false)} />

{#if session.status === "checking"}
  <div class="grid min-h-screen place-items-center">
    <Mark class="size-8 animate-pulse text-ink-faint" />
    <span class="sr-only">Checking the session</span>
  </div>
{:else if session.status === "unreachable"}
  <main class="flex min-h-screen items-center justify-center px-6">
    <div class="flex max-w-md flex-col items-center gap-4 text-center">
      <Mark class="size-9 text-ink-faint opacity-60" />
      <h1 class="font-cond text-xl font-bold tracking-[0.09em] uppercase">
        The server did not answer
      </h1>
      <p class="text-[13px] text-ink-mute">
        This page is served by the same process as the API, so if it loaded and the API did
        not, the server is shutting down or something between here and it is refusing the
        request.
      </p>
      <Button onclick={() => session.check()}>Try again</Button>
    </div>
  </main>
{:else if session.status === "anonymous"}
  <SignIn />
{:else}
  <!--
    The viewport is the shell: the rail and the command bar stay put and the
    content pane scrolls. Not a preference: a table's sticky header sticks to
    its nearest scroll container, so a page that scrolls instead puts the header
    over the first row and swallows its clicks.
  -->
  <!--
    On a narrow screen the rail would take half of it, so it waits behind a
    button in a bar of its own, and the bar keeps the one line about how this
    server stands that the rail would otherwise show.
  -->
  <div
    class="grid h-dvh grid-rows-[auto_minmax(0,1fr)] overflow-hidden
           md:grid-cols-[208px_minmax(0,1fr)] md:grid-rows-1"
  >
    <header
      class="flex items-center gap-3 border-b border-line bg-surface px-4 py-2.5 md:hidden"
    >
      <Brand />
      <button
        type="button"
        onclick={() => (menu = true)}
        aria-label="Open the sections"
        aria-expanded={menu}
        class="ml-auto grid size-9 cursor-pointer place-items-center rounded-sm text-ink-mute
               hover:bg-raised hover:text-ink"
      >
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" class="size-5">
          <path d="M4 7h16M4 12h16M4 17h16" stroke-linecap="round" />
        </svg>
      </button>
    </header>

    <div class={menu ? "fixed inset-0 z-40 flex md:static md:z-auto md:block" : "hidden md:block"}>
      <Rail />
      {#if menu}
        <button
          type="button"
          onclick={() => (menu = false)}
          aria-label="Close the sections"
          class="flex-1 cursor-default bg-sunken/70 backdrop-blur-[2px] md:hidden"
        ></button>
      {/if}
    </div>

    <main class="flex min-w-0 flex-col overflow-hidden">
      {#if stopped}
        <div
          role="status"
          class="flex flex-wrap items-baseline gap-x-3 gap-y-1 border-b border-line border-l-[3px]
                 border-l-crit bg-crit-lo px-5 py-2.5 text-[13px]"
        >
          <span class="font-cond text-[13px] font-bold tracking-[0.08em] text-crit uppercase">
            No writes
          </span>
          <span class="text-ink-mute">
            {standing.writes.reason}
          </span>
          <a href="/cluster" class="ml-auto text-[12px] text-ink underline-offset-2 hover:underline">
            Cluster
          </a>
        </div>
      {/if}
      {@render children()}
    </main>
  </div>

  <Palette />
{/if}
