<script lang="ts">
  /**
   * A comma-separated list held as one setting: the draft, the button that
   * commits it, and what is actually stored underneath.
   *
   * Two of these sit on the settings screen and they are the same decision
   * shape. Written twice they drifted apart in wording and spacing, which is
   * how a reader learns to check whether two things that look alike behave
   * alike.
   */
  import Button from "./Button.svelte";
  import Chip from "./Chip.svelte";
  import Field from "./Field.svelte";

  let {
    label,
    placeholder,
    value = $bindable(),
    stored,
    reads,
    empty,
    disabled = false,
    saving = false,
    onsave,
  }: {
    label: string;
    placeholder: string;
    value: string;
    /** What the server holds, which is not the draft in the field above it. */
    stored: string[];
    /** How the stored line opens, so it reads as a sentence. */
    reads: string;
    /** What an empty list means, said rather than left blank. */
    empty: string;
    disabled?: boolean;
    saving?: boolean;
    onsave: () => void;
  } = $props();
</script>

<div class="flex items-end gap-3">
  <div class="flex-1">
    <Field {label} bind:value {placeholder} disabled={disabled || saving} />
  </div>
  <Button weight="primary" disabled={disabled || saving} onclick={onsave}>
    {saving ? "Saving…" : "Save"}
  </Button>
</div>

<p class="flex flex-wrap items-center gap-2 text-[12px] text-ink-mute">
  <span>{reads}</span>
  {#if stored.length === 0}
    <Chip tone="warn">{empty}</Chip>
  {:else}
    {#each stored as entry (entry)}
      <Chip tone="ok">{entry}</Chip>
    {/each}
  {/if}
</p>
