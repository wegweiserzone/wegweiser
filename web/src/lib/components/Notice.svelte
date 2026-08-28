<script lang="ts">
  /**
   * A designed failure.
   */
  import type { Snippet } from "svelte";

  type Tone = "warn" | "crit" | "signal";

  let {
    tone = "warn",
    title,
    children,
    actions,
  }: { tone?: Tone; title: string; children: Snippet; actions?: Snippet } = $props();

  const rails: Record<Tone, string> = {
    warn: "border-l-warn",
    crit: "border-l-crit",
    signal: "border-l-signal",
  };
  const inks: Record<Tone, string> = {
    warn: "text-warn",
    crit: "text-crit",
    signal: "text-signal",
  };
</script>

<div
  class="flex items-start gap-3 rounded-sm border border-line border-l-2 bg-surface
         px-4 py-3 {rails[tone]}"
  role="alert"
>
  <svg
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.9"
    class="mt-px size-4 shrink-0 {inks[tone]}"
    aria-hidden="true"
  >
    <path d="M12 3 2.5 20h19L12 3Z" stroke-linejoin="round" />
    <path d="M12 9.5v5M12 17.5v.01" stroke-linecap="round" />
  </svg>

  <div class="flex min-w-0 flex-1 flex-col gap-1">
    <p class="font-cond text-[14px] font-bold tracking-[0.06em] uppercase">{title}</p>
    <div class="max-w-[70ch] text-[13px] text-ink-mute">{@render children()}</div>
  </div>

  {#if actions}
    <div class="flex shrink-0 items-center gap-1.5">{@render actions()}</div>
  {/if}
</div>
