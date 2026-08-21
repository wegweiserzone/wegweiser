<script lang="ts">
  /**
   * One number, with what it counts above it.
   */
  import type { Snippet } from "svelte";

  let {
    label,
    unit,
    tone,
    children,
  }: { label: string; unit?: string; tone?: "signal" | "ok" | "warn"; children: Snippet } =
    $props();

  const inks = { signal: "text-signal", ok: "text-ok", warn: "text-warn" };
</script>

<div class="flex min-w-[132px] shrink-0 flex-col gap-1 border-r border-line-soft px-5 py-3">
  <dt class="sign text-[10px] text-ink-faint">{label}</dt>
  <dd class="num text-[19px] leading-tight font-medium {tone ? inks[tone] : ''}">
    {@render children()}{#if unit}<small class="ml-1 text-[11px] font-normal text-ink-faint"
        >{unit}</small
      >{/if}
  </dd>
</div>
