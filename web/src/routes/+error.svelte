<script lang="ts">
  /**
   * The page that is not there.
   */
  import { page } from "$app/state";
  import Button from "$lib/components/Button.svelte";
  import Mark from "$lib/components/Mark.svelte";

  const missing = $derived(page.status === 404);
</script>

<svelte:head>
  <title>{page.status} — Wegweiser</title>
</svelte:head>

<main class="flex min-h-screen items-center justify-center px-6">
  <div class="flex max-w-md flex-col items-center gap-4 text-center">
    <Mark class="size-9 text-ink-faint opacity-60" />

    <div class="flex items-baseline gap-2.5">
      <span class="num text-[13px] text-ink-faint">{page.status}</span>
      <h1 class="font-cond text-xl font-bold tracking-[0.09em] uppercase">
        {missing ? "No such page" : "Something went wrong"}
      </h1>
    </div>

    {#if missing}
      <p class="text-[13px] text-ink-mute">
        The interface has no page at
        <code class="num text-ink">{page.url.pathname}</code>. A link that used to work may
        have moved, and a hand-typed address is easy to get wrong.
      </p>
    {:else}
      <p class="text-[13px] text-ink-mute">
        {page.error?.message ?? "The interface could not show this page."}
      </p>
    {/if}

    <Button onclick={() => (location.href = "/")}>Back to the overview</Button>
  </div>
</main>
