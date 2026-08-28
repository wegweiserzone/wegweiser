<script lang="ts">
  /**
   * A designed nothing.
   */
  import type { Snippet } from "svelte";
  import Mark from "./Mark.svelte";

  let {
    title,
    children,
    actions,
  }: { title: string; children?: Snippet; actions?: Snippet } = $props();
</script>

<div class="flex flex-col items-center gap-3 text-center">
  <Mark class="size-9 text-ink-faint opacity-40" />
  <h3 class="font-cond text-lg font-bold tracking-[0.09em] uppercase">{title}</h3>
  {#if children}
    <p class="max-w-[48ch] text-[13px] text-ink-mute">{@render children()}</p>
  {/if}
  {#if actions}
    <div class="mt-1 flex gap-2">{@render actions()}</div>
  {/if}
</div>
