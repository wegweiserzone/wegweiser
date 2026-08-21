<script lang="ts">
  /**
   * A labelled input.
   */
  import type { HTMLInputAttributes } from "svelte/elements";

  interface Props extends Omit<HTMLInputAttributes, "value"> {
    label: string;
    hint?: string;
    value?: string;
  }

  let { label, hint, value = $bindable(""), id, ...rest }: Props = $props();

  // Derived rather than computed once: the label is a prop, and a component
  // whose label changes would otherwise keep pointing the <label> at an id
  // that no longer describes it.
  const fieldId = $derived(id ?? `field-${label.toLowerCase().replace(/[^a-z0-9]+/g, "-")}`);
</script>

<div class="flex flex-col gap-1.5">
  <label for={fieldId} class="sign text-[11px] text-ink-faint">{label}</label>
  <input
    id={fieldId}
    bind:value
    class="num h-9 w-full rounded-sm border border-line bg-surface px-3 text-[13px]
           text-ink transition-colors outline-none placeholder:text-ink-faint
           focus:border-signal disabled:cursor-not-allowed disabled:bg-raised
           disabled:text-ink-mute"
    {...rest}
  />
  {#if hint}
    <p class="text-xs text-ink-mute">{hint}</p>
  {/if}
</div>
