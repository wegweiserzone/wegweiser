<script lang="ts">
  /**
   * A record's data, as the parts it is made of.
   */
  import { assemble, disassemble, shapeOf } from "$lib/rdata";
  import Field from "./Field.svelte";

  let {
    type,
    value = $bindable(""),
    disabled = false,
  }: { type: string; value?: string; disabled?: boolean } = $props();

  const shape = $derived(shapeOf(type));

  /** Whether the person asked for the line itself rather than the parts. */
  let raw = $state(false);

  /** The parts, kept separately so an empty one does not lose its place. */
  let parts = $state<string[]>([]);

  // Re-read the parts whenever the type changes or the value arrives from
  // outside: opening the editor on an existing record is the case that
  // matters, and it is the only one that writes `value` from elsewhere.
  let lastType = $state("");
  let lastValue = $state("");
  $effect(() => {
    if (type === lastType && value === lastValue) return;
    lastType = type;
    lastValue = value;
    parts = shape ? disassemble(shape, value) : [];
  });

  function edit(index: number, next: string) {
    if (!shape) return;
    parts[index] = next;
    value = assemble(shape, parts);
    lastValue = value;
  }

  const preview = $derived(value.trim());
</script>

{#if shape && !raw}
  <div class="flex flex-col gap-3">
    <div class="grid gap-3 {shape.parts.length > 2 ? 'sm:grid-cols-2' : ''}">
      {#each shape.parts as part, i (part.label)}
        {#if part.options}
          <div class="flex flex-col gap-1.5">
            <label for="rdata-{i}" class="sign text-[11px] text-ink-faint">{part.label}</label>
            <input
              id="rdata-{i}"
              list="rdata-{i}-options"
              value={parts[i] ?? ""}
              {disabled}
              oninput={(e) => edit(i, e.currentTarget.value)}
              placeholder={part.placeholder}
              autocomplete="off"
              spellcheck="false"
              class="num h-9 w-full rounded-sm border border-line bg-surface px-3 text-[13px]
                     text-ink outline-none transition-colors focus:border-signal
                     disabled:cursor-not-allowed disabled:bg-raised"
            />
            <datalist id="rdata-{i}-options">
              {#each part.options as option (option)}
                <option value={option}></option>
              {/each}
            </datalist>
            {#if part.hint}<p class="text-xs text-ink-mute">{part.hint}</p>{/if}
          </div>
        {:else}
          <Field
            id="rdata-{i}"
            label={part.label}
            hint={part.hint}
            placeholder={part.placeholder}
            inputmode={part.number ? "numeric" : undefined}
            autocomplete="off"
            spellcheck={false}
            {disabled}
            value={parts[i] ?? ""}
            oninput={(e) => edit(i, e.currentTarget.value)}
          />
        {/if}
      {/each}
    </div>

    <div class="flex items-baseline gap-2">
      <span class="sign shrink-0 text-[10px] text-ink-faint">becomes</span>
      <code class="num min-w-0 flex-1 truncate text-[12px] text-ink-mute" title={preview}>
        {preview || shape.example}
      </code>
      <button
        type="button"
        onclick={() => (raw = true)}
        {disabled}
        class="sign shrink-0 cursor-pointer text-[10px] text-ink-faint underline-offset-2
               hover:text-ink hover:underline disabled:cursor-not-allowed"
      >
        edit as one line
      </button>
    </div>
  </div>
{:else}
  <div class="flex flex-col gap-1.5">
    <Field
      label="Data"
      bind:value
      {disabled}
      placeholder={shape?.example ?? "in presentation format"}
      autocomplete="off"
      spellcheck={false}
      hint="In presentation format, the way a zonefile writes it."
    />
    {#if shape}
      <button
        type="button"
        onclick={() => {
          raw = false;
          lastValue = "";
        }}
        {disabled}
        class="sign cursor-pointer self-start text-[10px] text-ink-faint underline-offset-2
               hover:text-ink hover:underline disabled:cursor-not-allowed"
      >
        back to the fields
      </button>
    {/if}
  </div>
{/if}
