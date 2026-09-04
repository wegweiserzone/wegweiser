<script lang="ts">
  /**
   * A modal, on the platform's own dialog element.
   */
  import type { Snippet } from "svelte";

  let {
    open = $bindable(false),
    title,
    size = "default",
    children,
    actions,
    onclose,
  }: {
    open?: boolean;
    title: string;
    /**
     * How wide. The default is a question and its answer; wide is for a dialog
     * that has to show something written rather than ask something.
     */
    size?: "default" | "wide";
    children: Snippet;
    actions?: Snippet;
    /** Called however the dialog was dismissed, including by the escape key. */
    onclose?: () => void;
  } = $props();

  const widths: Record<"default" | "wide", string> = {
    default: "w-[min(30rem,calc(100vw-2rem))]",
    wide: "w-[min(48rem,calc(100vw-2rem))]",
  };

  let element = $state<HTMLDialogElement | null>(null);

  $effect(() => {
    if (!element) return;
    if (open && !element.open) element.showModal();
    if (!open && element.open) element.close();
  });
</script>

<dialog
  bind:this={element}
  onclose={() => {
    open = false;
    onclose?.();
  }}
  class="m-auto max-h-[85vh] rounded-md border border-line bg-surface p-0 text-ink
         shadow-[var(--shadow-lift)] backdrop:bg-sunken/70 backdrop:backdrop-blur-[3px]
         {widths[size]}"
>
  {#if open}
    <div class="flex flex-col">
      <h2
        class="font-cond border-b border-line px-5 py-3.5 text-[15px] font-bold
               tracking-[0.1em] uppercase"
      >
        {title}
      </h2>

      <div class="flex flex-col gap-4 px-5 py-4">
        {@render children()}
      </div>

      {#if actions}
        <div class="flex justify-end gap-2 border-t border-line bg-raised px-5 py-3">
          {@render actions()}
        </div>
      {/if}
    </div>
  {/if}
</dialog>
