<script lang="ts">
  /**
   * What a write did besides what was asked for.
   *
   * This is the whole reverse story in one place: the PTRs that were written
   * without being asked for, the ones that were not because another name
   * already claims the address (D3), and the reverse zones that would have to
   * exist first (D6). None of it is an error and none of it is a log line —
   * it is the answer to "what just happened", and hiding it is what makes
   * automation feel like something being done to you.
   */
  import type { Conflict, MissingZone, Record_ } from "$lib/api";
  import Chip from "./Chip.svelte";

  let {
    generated = [],
    conflicts = [],
    missingZones = [],
  }: {
    generated?: Record_[];
    conflicts?: Conflict[];
    missingZones?: MissingZone[];
  } = $props();

  const anything = $derived(
    generated.length > 0 || conflicts.length > 0 || missingZones.length > 0,
  );
</script>

{#if anything}
  <div class="flex flex-col gap-2 rounded-sm border border-line bg-raised px-3 py-2.5">
    {#each generated as record (record.id)}
      <p class="flex flex-wrap items-baseline gap-2 text-[12px]">
        <Chip tone="signal">Written</Chip>
        <span class="num">{record.name}</span>
        <span class="num text-ink-faint">{record.ttl} IN {record.type}</span>
        <span class="num">{record.data}</span>
      </p>
    {/each}

    {#each conflicts as clash (clash.address)}
      <p class="flex flex-wrap items-baseline gap-2 text-[12px]">
        <Chip tone="warn">Kept</Chip>
        <span class="text-ink-mute">
          <span class="num text-ink">{clash.address}</span> still points at
          <span class="num text-ink">{clash.existingName}</span>, not
          <span class="num text-ink">{clash.requestedName}</span>. The first record keeps the
          name; nothing was overwritten.
        </span>
      </p>
    {/each}

    {#each missingZones as gap (gap.zoneName)}
      <p class="flex flex-wrap items-baseline gap-2 text-[12px]">
        <Chip>No zone</Chip>
        <span class="text-ink-mute">
          <span class="num text-ink">{gap.address}</span> has no reverse here: it would need
          <span class="num text-ink">{gap.zoneName}</span>. Create it and the entry is filled
          in.
        </span>
      </p>
    {/each}
  </div>
{/if}
