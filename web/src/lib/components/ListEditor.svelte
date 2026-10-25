<script lang="ts">
  /**
   * A short list edited in place: each value a mark that can be taken away,
   * and a field that adds another.
   *
   * For lists that are a handful of exact strings, such as the groups given a
   * scope or the redirect URIs a provider holds. A comma-separated field would
   * hide where one value ends, and a value with a typo in it would sit inside
   * a line rather than on its own where it can be seen.
   */
  import Button from "./Button.svelte";

  let {
    label,
    items = $bindable(),
    placeholder,
    empty,
    disabled = false,
  }: {
    /** What the list is, for the screen reader and the add field. */
    label: string;
    items: string[];
    placeholder: string;
    /** What an empty list means, said rather than left blank. */
    empty: string;
    disabled?: boolean;
  } = $props();

  let draft = $state("");

  // Compared without case, as the server compares a group, so that the list
  // never holds two values the server would read as one.
  const held = (value: string) => items.some((i) => i.toLowerCase() === value.toLowerCase());

  function add(event?: Event) {
    event?.preventDefault();
    const value = draft.trim();
    if (!value) return;
    if (!held(value)) items = [...items, value];
    draft = "";
  }
</script>

<div class="flex flex-col gap-2">
  <ul class="flex flex-wrap gap-1.5" aria-label={label}>
    {#each items as item (item)}
      <li
        class="num inline-flex max-w-full items-center gap-1 rounded-xs border border-line
               bg-raised py-0.5 pr-0.5 pl-2 text-[12px] text-ink"
      >
        <span class="break-all">{item}</span>
        {#if !disabled}
          <button
            type="button"
            aria-label="Remove {item}"
            onclick={() => (items = items.filter((i) => i !== item))}
            class="grid size-5 shrink-0 cursor-pointer place-items-center rounded-xs text-ink-faint
                   transition-colors hover:bg-crit-lo hover:text-crit"
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class="size-3">
              <path d="m7 7 10 10M17 7 7 17" stroke-linecap="round" />
            </svg>
          </button>
        {/if}
      </li>
    {:else}
      <li class="text-[12px] text-ink-faint">{empty}</li>
    {/each}
  </ul>

  {#if !disabled}
    <div class="flex gap-2">
      <input
        aria-label="Add to {label}"
        bind:value={draft}
        onkeydown={(e) => e.key === "Enter" && add(e)}
        {placeholder}
        spellcheck={false}
        autocomplete="off"
        class="num h-8 min-w-0 flex-1 rounded-sm border border-line bg-surface px-3 text-[13px]
               text-ink transition-colors outline-none placeholder:text-ink-faint
               focus:border-signal"
      />
      <Button type="button" onclick={add} disabled={!draft.trim() || held(draft.trim())}>Add</Button>
    </div>
  {/if}
</div>
