<script lang="ts">
  /**
   * Everything the interface shows once there is somebody to show it to.
   */
  import { page } from "$app/state";
  import { session } from "$lib/session.svelte";
  import { standing } from "$lib/standing.svelte";
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
</script>

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
  <div class="grid h-screen grid-cols-[208px_minmax(0,1fr)] overflow-hidden">
    <Rail />
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
