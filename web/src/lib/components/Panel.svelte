<script lang="ts">
  /**
   * One block of a settings screen: what it is, why it is there, and the
   * controls for it.
   *
   * A page of these reads as a list of decisions. The same content separated
   * only by a gap reads as one long column that has to be scanned to find where
   * anything begins.
   */
  import type { Snippet } from "svelte";

  let {
    heading,
    blurb,
    children,
  }: { heading: string; blurb?: Snippet; children: Snippet } = $props();
</script>

<section class="max-w-3xl overflow-hidden rounded-sm border border-line bg-surface">
  <div class="flex flex-col gap-1.5 border-b border-line bg-raised px-4 py-3">
    <h2 class="font-cond text-[13px] font-bold tracking-[0.1em] text-ink uppercase">
      {heading}
    </h2>
    {#if blurb}
      <p class="text-[12px] text-ink-mute">{@render blurb()}</p>
    {/if}
  </div>
  <div class="flex flex-col gap-4 px-4 py-4">{@render children()}</div>
</section>
