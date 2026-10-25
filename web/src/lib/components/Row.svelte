<script lang="ts">
  /**
   * One setting: on the left what it is and why it matters, on the right the
   * control. On a narrow screen the two stack.
   *
   * The control's column stops widening at a width an input can still be read
   * at, so that a field for an address does not stretch across a large screen.
   */
  import type { Snippet } from "svelte";

  let {
    title,
    about,
    children,
  }: { title: string; about?: Snippet; children: Snippet } = $props();
</script>

<section class="grid gap-x-10 gap-y-3 py-5 md:grid-cols-[minmax(11rem,19rem)_minmax(0,1fr)]">
  <div class="flex flex-col gap-1">
    <h3 class="text-[14px] text-ink">{title}</h3>
    {#if about}
      <p class="text-[12px] leading-relaxed text-ink-mute">{@render about()}</p>
    {/if}
  </div>
  <div class="flex min-w-0 max-w-4xl flex-col gap-3">{@render children()}</div>
</section>
