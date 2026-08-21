<script lang="ts">
  /**
   * A state mark. Small, condensed, uppercase: readable before the words
   * around it are.
   */
  import type { Snippet } from "svelte";

  type Tone = "neutral" | "ok" | "warn" | "crit" | "info" | "signal";

  let {
    tone = "neutral",
    dot = false,
    title,
    children,
  }: { tone?: Tone; dot?: boolean; title?: string; children: Snippet } = $props();

  const tones: Record<Tone, string> = {
    neutral: "bg-raised text-ink-mute",
    ok: "bg-ok-lo text-ok",
    warn: "bg-warn-lo text-warn",
    crit: "bg-crit-lo text-crit",
    info: "bg-info-lo text-info",
    signal: "bg-signal-lo text-signal",
  };
</script>

<span
  {title}
  class="sign inline-flex items-center gap-1.5 rounded-xs px-1.5 py-px text-[11px]
         whitespace-nowrap {tones[tone]}"
>
  {#if dot}
    <span class="size-1.5 shrink-0 rounded-full bg-current"></span>
  {/if}
  {@render children()}
</span>
