<script lang="ts">
  /**
   * What one commit changed, written the way a zonefile writes records.
   *
   * There is no "modify" here because there is none in the journal: a change
   * is a removal and an addition, which is how RFC 1995 §2 expresses it too,
   * and both carry the full record so either direction can be replayed.
   * Removals come first for the same reason.
   */
  import type { Event } from "$lib/api";

  let { events }: { events: Event[] } = $props();

  /** width lines the columns up without a table, the way a zonefile does. */
  function line(event: Event): string {
    return `${event.name.padEnd(34)} ${String(event.ttl).padStart(6)} ${event.class} ${event.type.padEnd(6)} ${event.data}`;
  }
</script>

<div class="num text-[12.5px] leading-relaxed">
  {#each events as event (event.seq)}
    <div
      class="grid grid-cols-[1.25rem_minmax(0,1fr)] whitespace-pre {event.op === 'add'
        ? 'bg-[color-mix(in_srgb,var(--ok)_11%,transparent)]'
        : 'bg-[color-mix(in_srgb,var(--crit)_11%,transparent)]'}"
    >
      <span
        class="text-center select-none {event.op === 'add' ? 'text-ok' : 'text-crit'}"
        aria-hidden="true"
      >
        {event.op === "add" ? "+" : "−"}
      </span>
      <span class="overflow-x-auto pr-5">{line(event)}</span>
    </div>
  {/each}
</div>
