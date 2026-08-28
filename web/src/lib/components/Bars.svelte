<script lang="ts">
  /**
   * A handful of named counts, as bars.
   */
  import type { Reading } from "$lib/metrics";

  let {
    readings,
    total,
    tone,
    empty = "nothing yet",
  }: {
    readings: Reading[];
    total: number;
    /** Given a label, which state colour it stands for. */
    tone?: (label: string) => "ok" | "warn" | "crit" | "signal" | "neutral";
    empty?: string;
  } = $props();

  const fills: Record<string, string> = {
    ok: "bg-ok",
    warn: "bg-warn",
    crit: "bg-crit",
    signal: "bg-signal",
    neutral: "bg-ink-faint",
  };

  const share = (count: number) => (total === 0 ? 0 : (count / total) * 100);
</script>

{#if readings.length === 0}
  <p class="text-[13px] text-ink-faint">{empty}</p>
{:else}
  <dl class="flex flex-col gap-1.5">
    {#each readings as reading (reading.label)}
      <div class="grid grid-cols-[6rem_minmax(0,1fr)_5.5rem] items-center gap-3">
        <dt class="num truncate text-[12px]" title={reading.label}>{reading.label}</dt>
        <dd class="h-1.5 overflow-hidden rounded-full bg-raised">
          <div
            class="h-full rounded-full transition-[width] duration-200 ease-signal
                   {fills[tone?.(reading.label) ?? 'neutral']}"
            style="width: {Math.max(share(reading.count), reading.count > 0 ? 1.5 : 0)}%"
          ></div>
        </dd>
        <dd class="num text-right text-[12px] text-ink-mute">
          {reading.count.toLocaleString("en")}
          <span class="text-ink-faint">{share(reading.count).toFixed(0)}%</span>
        </dd>
      </div>
    {/each}
  </dl>
{/if}
