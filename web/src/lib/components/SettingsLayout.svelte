<script lang="ts">
  /**
   * A settings page: its sections one under the other, and beside them on a
   * wide screen a list that jumps to each
   * (docs/decisions/d53-how-the-interface-is-laid-out.md).
   *
   * It is the pane that scrolls, so the list stays where it is while the
   * sections move. A page of one or two sections passes none and gets no list.
   */
  import type { Snippet } from "svelte";

  let {
    sections = [],
    children,
  }: { sections?: { id: string; label: string }[]; children: Snippet } = $props();
</script>

<div class="min-h-0 flex-1 overflow-auto">
  <div
    class="grid gap-x-10 gap-y-6 px-5 py-6 sm:px-8
           {sections.length > 0 ? 'lg:grid-cols-[10rem_minmax(0,1fr)]' : ''}"
  >
    {#if sections.length > 0}
      <nav aria-label="On this page" class="hidden lg:block">
        <ul class="sticky top-0 flex flex-col gap-px">
          {#each sections as s (s.id)}
            <li>
              <a
                href="#{s.id}"
                class="block rounded-sm px-2.5 py-1.5 text-[13px] text-ink-mute transition-colors
                       hover:bg-raised hover:text-ink"
              >
                {s.label}
              </a>
            </li>
          {/each}
        </ul>
      </nav>
    {/if}
    <div class="flex min-w-0 flex-col gap-12">{@render children()}</div>
  </div>
</div>
