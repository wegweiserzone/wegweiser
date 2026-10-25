<script lang="ts">
  /**
   * A group of settings under one heading, its rows divided by a hairline.
   * No box around it: the heading and the rule are what tell one group from
   * the next, so the settings themselves carry the page. It is a region
   * rather than a section element: each of its rows is a section of its own,
   * one setting with its heading.
   */
  import type { Snippet } from "svelte";

  let {
    id,
    heading,
    blurb,
    children,
  }: {
    /** What the page's list of sections jumps to. */
    id?: string;
    heading: string;
    /** A sentence about the whole group, when one is needed. */
    blurb?: Snippet;
    children: Snippet;
  } = $props();
</script>

<div {id} role="region" aria-label={heading} class="flex scroll-mt-6 flex-col">
  <h2
    class="border-b border-line pb-2.5 font-cond text-[15px] font-bold tracking-[0.08em] uppercase"
  >
    {heading}
  </h2>
  {#if blurb}
    <p class="max-w-[72ch] pt-3 text-[13px] text-ink-mute">{@render blurb()}</p>
  {/if}
  <div class="flex flex-col divide-y divide-line-soft">{@render children()}</div>
</div>
