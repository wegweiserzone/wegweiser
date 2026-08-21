<script lang="ts">
  /**
   * Moving through a listing.
   */
  let {
    page = $bindable(0),
    size = $bindable(250),
    shown,
    hasNext,
    busy = false,
    onchange,
  }: {
    page?: number;
    size?: number;
    /** How many rows this page actually holds. */
    shown: number;
    hasNext: boolean;
    busy?: boolean;
    onchange: (next: { page: number; size: number }) => void;
  } = $props();

  const sizes = [100, 250, 500, 1000];

  // Positions rather than a total: every page before this one was full, so
  // this arithmetic is exact even though nothing counted the rows.
  const from = $derived(shown === 0 ? 0 : page * size + 1);
  const to = $derived(page * size + shown);
</script>

<div class="flex shrink-0 items-center gap-3 border-t border-line bg-surface px-5 py-2">
  <span class="num text-[11px] text-ink-faint">
    {#if shown === 0}
      nothing here
    {:else}
      {from}–{to}
    {/if}
  </span>

  <label class="ml-auto flex items-center gap-2">
    <span class="sign text-[10px] text-ink-faint">per page</span>
    <select
      value={size}
      onchange={(e) => onchange({ page: 0, size: Number(e.currentTarget.value) })}
      class="num h-7 cursor-pointer rounded-sm border border-line bg-ground px-2 text-[12px]
             text-ink outline-none focus:border-signal"
    >
      {#each sizes as option (option)}
        <option value={option}>{option}</option>
      {/each}
    </select>
  </label>

  <div class="flex items-center gap-1">
    <button
      type="button"
      disabled={page === 0 || busy}
      onclick={() => onchange({ page: page - 1, size })}
      class="sign flex h-7 cursor-pointer items-center rounded-sm border border-line bg-ground
             px-2.5 text-[11px] text-ink transition-colors hover:bg-raised
             disabled:cursor-not-allowed disabled:text-ink-faint disabled:hover:bg-ground"
    >
      Previous
    </button>

    <span class="num px-1 text-[11px] text-ink-mute">page {page + 1}</span>

    <button
      type="button"
      disabled={!hasNext || busy}
      onclick={() => onchange({ page: page + 1, size })}
      class="sign flex h-7 cursor-pointer items-center rounded-sm border border-line bg-ground
             px-2.5 text-[11px] text-ink transition-colors hover:bg-raised
             disabled:cursor-not-allowed disabled:text-ink-faint disabled:hover:bg-ground"
    >
      Next
    </button>
  </div>
</div>
