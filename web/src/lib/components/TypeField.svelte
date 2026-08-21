<script lang="ts">
  /**
   * The record type: a list you can see, and a field you can type anything
   * into.
   *
   * This shows the whole set, grouped by what the types are for, and narrows
   * as you type. It never restricts: the server stores anything with a
   * mnemonic and anything without one in the TYPE<number> form of RFC 3597,
   * and a control that refused those would be lying about what it can hold.
   */
  import { suggestedTypes } from "$lib/records";

  let {
    value = $bindable(""),
    id = "record-type",
    label = "Type",
  }: { value?: string; id?: string; label?: string } = $props();

  let open = $state(false);
  let cursor = $state(0);
  let input = $state<HTMLInputElement | null>(null);

  /** typed is what narrows the list, but only once it differs from the value. */
  let typed = $state("");

  const groups = $derived.by(() => {
    const needle = typed.trim().toUpperCase();
    if (!needle) return suggestedTypes;
    return suggestedTypes
      .map((g) => ({ label: g.label, types: g.types.filter((t) => t.includes(needle)) }))
      .filter((g) => g.types.length > 0);
  });

  const flat = $derived(groups.flatMap((g) => g.types));

  function choose(type: string) {
    value = type;
    typed = "";
    open = false;
    input?.focus();
  }

  function onKey(event: KeyboardEvent) {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      open = true;
      const step = event.key === "ArrowDown" ? 1 : -1;
      cursor = Math.max(0, Math.min(flat.length - 1, cursor + step));
      return;
    }
    const picked = flat[cursor];
    if (event.key === "Enter" && open && picked !== undefined) {
      event.preventDefault();
      choose(picked);
      return;
    }
    if (event.key === "Escape" && open) {
      event.preventDefault();
      open = false;
    }
  }
</script>

<svelte:window
  onpointerdown={(e) => {
    if (open && e.target instanceof Node && !input?.parentElement?.contains(e.target)) {
      open = false;
    }
  }}
/>

<div class="flex flex-col gap-1.5">
  <label for={id} class="sign text-[11px] text-ink-faint">{label}</label>

  <div class="relative">
    <input
      {id}
      bind:this={input}
      value={value}
      role="combobox"
      aria-expanded={open}
      aria-controls="{id}-list"
      aria-autocomplete="list"
      autocomplete="off"
      spellcheck="false"
      oninput={(e) => {
        value = e.currentTarget.value.toUpperCase();
        typed = value;
        open = true;
        cursor = 0;
      }}
      onfocus={() => ((open = true), (typed = ""), (cursor = 0))}
      onkeydown={onKey}
      class="num h-9 w-full rounded-sm border border-line bg-surface pr-8 pl-3 text-[13px]
             text-ink uppercase transition-colors outline-none focus:border-signal"
    />

    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      class="pointer-events-none absolute top-1/2 right-2.5 size-3.5 -translate-y-1/2 text-ink-faint"
      aria-hidden="true"
    >
      <path d="m6 9 6 6 6-6" stroke-linecap="round" stroke-linejoin="round" />
    </svg>

    {#if open}
      <ul
        id="{id}-list"
        role="listbox"
        class="absolute z-30 mt-1 max-h-64 w-full overflow-y-auto rounded-sm border border-line
               bg-surface py-1 shadow-[var(--shadow-lift)]"
      >
        {#each groups as group (group.label)}
          <li class="sign px-2.5 pt-2 pb-1 text-[10px] text-ink-faint" role="presentation">
            {group.label}
          </li>
          {#each group.types as type (type)}
            {@const index = flat.indexOf(type)}
            <li role="presentation">
              <button
                type="button"
                role="option"
                aria-selected={type === value}
                onmouseenter={() => (cursor = index)}
                onclick={() => choose(type)}
                class="num flex w-full cursor-pointer items-center px-2.5 py-1 text-left text-[12px]
                       {index === cursor ? 'bg-signal-lo text-signal' : 'text-ink'}"
              >
                {type}
              </button>
            </li>
          {/each}
        {:else}
          <li class="px-2.5 py-2 text-[12px] text-ink-mute">
            No suggestion matches. It is still accepted: any type is.
          </li>
        {/each}
      </ul>
    {/if}
  </div>

  <p class="text-xs text-ink-mute">
    Any type is accepted, including the <code class="num text-ink">TYPE65534</code> form of
    RFC 3597 for one with no mnemonic.
  </p>
</div>
