<script lang="ts">
  /**
   * A labelled input.
   */
  import type { HTMLInputAttributes } from "svelte/elements";

  interface Props extends Omit<HTMLInputAttributes, "value"> {
    label: string;
    hint?: string;
    /** What is wrong with the value. Said where the hint is, in its place. */
    problem?: string;
    value?: string;
  }

  let { label, hint, problem, value = $bindable(""), id, ...rest }: Props = $props();

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
    aria-invalid={problem ? true : undefined}
    aria-describedby={problem || hint ? `${fieldId}-said` : undefined}
    class="num h-9 w-full rounded-sm border bg-surface px-3 text-[13px] text-ink
           transition-colors outline-none placeholder:text-ink-faint focus:border-signal
           disabled:cursor-not-allowed disabled:bg-raised disabled:text-ink-mute
           {problem ? 'border-crit' : 'border-line'}"
    {...rest}
  />
  {#if problem}
    <p id="{fieldId}-said" class="text-xs text-crit">{problem}</p>
  {:else if hint}
    <p id="{fieldId}-said" class="text-xs text-ink-mute">{hint}</p>
  {/if}
</div>
