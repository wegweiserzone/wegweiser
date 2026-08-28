<script lang="ts">
  /**
   * A failure inside the interface, shown inside it.
   */
  import { page } from "$app/state";
  import Button from "$lib/components/Button.svelte";
  import Empty from "$lib/components/Empty.svelte";

  const missing = $derived(page.status === 404);
</script>

<svelte:head><title>{page.status} — Wegweiser</title></svelte:head>

<div class="flex flex-1 items-center justify-center overflow-auto px-5 py-16">
  <Empty title={missing ? "Not here" : "Something went wrong"}>
    {page.error?.message ?? "The interface could not show this."}
    {#snippet actions()}
      <Button onclick={() => history.back()}>Go back</Button>
      <Button weight="quiet" onclick={() => location.reload()}>Try again</Button>
    {/snippet}
  </Empty>
</div>
