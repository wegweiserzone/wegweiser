<script lang="ts">
  /**
   * The command bar: what you are looking at, and what you can do to it.
   */
  import type { Snippet } from "svelte";

  let {
    title,
    subject,
    actions,
  }: { title: string; subject?: string; actions?: Snippet } = $props();
</script>

<header
  class="flex h-[54px] shrink-0 items-center gap-3 border-b border-line bg-ground px-5"
>
  <div class="flex min-w-0 items-baseline gap-2">
    <h1 class="font-cond text-[15px] font-bold tracking-[0.11em] uppercase">{title}</h1>
    {#if subject}
      <span class="text-ink-faint">/</span>
      <span class="num truncate text-[13px] text-ink-mute">{subject}</span>
    {/if}
  </div>

  {#if actions}
    <div class="ml-auto flex items-center gap-2">{@render actions()}</div>
  {/if}

  <!-- Discoverable without being told: a shortcut nobody knows about is a
       shortcut nobody uses. -->
  <kbd
    class="num ml-2 hidden shrink-0 rounded-xs border border-line border-b-2 bg-raised px-1.5
           py-0.5 text-[10px] text-ink-faint sm:block"
    title="Open the commands"
  >
    Ctrl K
  </kbd>
</header>
