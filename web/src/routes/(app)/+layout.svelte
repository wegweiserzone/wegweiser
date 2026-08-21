<script lang="ts">
  /**
   * Everything the interface shows once there is somebody to show it to.
   */
  import { api } from "$lib/api";
  import type { Health } from "$lib/api";
  import { session } from "$lib/session.svelte";
  import Button from "$lib/components/Button.svelte";
  import Mark from "$lib/components/Mark.svelte";
  import Palette from "$lib/components/Palette.svelte";
  import Rail from "$lib/components/Rail.svelte";
  import SignIn from "$lib/components/SignIn.svelte";

  let { children } = $props();

  let health = $state<Health | null>(null);

  session.check();

  $effect(() => {
    if (session.status !== "authenticated") {
      health = null;
      return;
    }
    api
      .get("/healthz")
      .then((h) => (health = h))
      .catch(() => (health = null));
  });
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
    <Rail {health} />
    <main class="flex min-w-0 flex-col overflow-hidden">
      {@render children()}
    </main>
  </div>

  <Palette />
{/if}
