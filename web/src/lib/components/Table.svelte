<script lang="ts" module>
  /** Column describes one column's heading and how it is read. */
  export interface Column {
    label: string;
    /** Numbers and anything else read from the right-hand edge. */
    align?: "right";
    /** A width the browser should not fight over, for columns that are stable. */
    width?: string;
  }
</script>

<script lang="ts" generics="Row">
  /**
   * The dense table everything in this interface is listed in.
   */
  import type { Snippet } from "svelte";

  let {
    columns,
    items,
    key,
    row,
    empty,
  }: {
    columns: Column[];
    items: Row[];
    key: (item: Row) => string;
    row: Snippet<[Row]>;
    empty?: Snippet;
  } = $props();
</script>

<table class="w-full border-collapse text-[13px]">
    <thead>
      <tr>
        {#each columns as column (column.label)}
          <th
            scope="col"
            style={column.width ? `width:${column.width}` : undefined}
            class="sign sticky top-0 z-10 border-b border-line bg-ground px-3 py-2
                   text-[11px] whitespace-nowrap text-ink-faint select-none
                   {column.align === 'right' ? 'text-right' : 'text-left'}
                   first:pl-5 last:pr-5"
          >
            {column.label}
          </th>
        {/each}
      </tr>
    </thead>

    <tbody>
      {#each items as item (key(item))}
        <tr class="group border-b border-line-soft transition-colors hover:bg-surface">
          {@render row(item)}
        </tr>
      {:else}
        <tr>
          <td colspan={columns.length} class="px-5 py-16">
            {#if empty}{@render empty()}{/if}
          </td>
        </tr>
      {/each}
    </tbody>
  </table>
