<script lang="ts">
  /**
   * A failure inside the interface, shown inside it.
   */
  import { goto } from "$app/navigation";
  import { page } from "$app/state";
  import Button from "$lib/components/Button.svelte";
  import Empty from "$lib/components/Empty.svelte";

  const missing = $derived(page.status === 404);
  const title = $derived(
    page.error?.zone ? "Not here" : missing ? "No such page" : "Something went wrong",
  );
</script>

<svelte:head><title>{page.status} — Wegweiser</title></svelte:head>

<div class="flex flex-1 items-center justify-center overflow-auto px-5 py-16">
  <Empty {title}>
    {#if page.error?.zone}
      There is no zone <span class="num text-ink">{page.error.zone}</span> on this server.
      {#if page.error.nearest}
        Did you mean
        <a href="/zones/{page.error.nearest}" class="num text-signal hover:underline"
          >{page.error.nearest}</a
        >?
      {/if}
    {:else if missing}
      The interface has no page at
      <code class="num text-ink">{page.url.pathname}</code>. A link that used to work may have
      moved, and a hand-typed address is easy to get wrong.
    {:else}
      {page.error?.message ?? "The interface could not show this."}
    {/if}
    {#snippet actions()}
      {#if page.error?.zone}
        <!-- Trying again would ask for the same name and get the same answer. -->
        <Button onclick={() => goto("/zones")}>All zones</Button>
      {:else if missing}
        <Button onclick={() => goto("/")}>Back to the overview</Button>
      {:else}
        <Button onclick={() => history.back()}>Go back</Button>
        <Button weight="quiet" onclick={() => location.reload()}>Try again</Button>
      {/if}
    {/snippet}
  </Empty>
</div>
