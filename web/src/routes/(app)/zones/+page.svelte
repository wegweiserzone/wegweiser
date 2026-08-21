<script lang="ts">
  /**
   * Every zone this server answers for.
   */
  import { untrack } from "svelte";

  import { goto } from "$app/navigation";
  import { api, ApiError, NetworkError } from "$lib/api";
  import type { Zone } from "$lib/api";
  import { ago, exact } from "$lib/format";
  import { session } from "$lib/session.svelte";
  import Bar from "$lib/components/Bar.svelte";
  import Button from "$lib/components/Button.svelte";
  import Chip from "$lib/components/Chip.svelte";
  import Dialog from "$lib/components/Dialog.svelte";
  import Empty from "$lib/components/Empty.svelte";
  import Field from "$lib/components/Field.svelte";
  import Notice from "$lib/components/Notice.svelte";
  import Pager from "$lib/components/Pager.svelte";
  import Table from "$lib/components/Table.svelte";
  import type { Column } from "$lib/components/Table.svelte";

  /**
   * The listing is the server's.
   */
  let zones = $state<Zone[]>([]);
  let loading = $state(true);
  let trouble = $state<string | null>(null);

  let search = $state("");
  let kind = $state<"" | "forward" | "reverse">("");
  let pageIndex = $state(0);
  let pageSize = $state(250);
  /** The cursor that opens each page. The first page has none. */
  let cursors = $state<(string | undefined)[]>([undefined]);
  let nextCursor = $state<string | undefined>(undefined);

  let creating = $state(false);
  let newName = $state("");
  let newTtl = $state("");
  let newNsAddress = $state("");
  let refused = $state<string | null>(null);
  let working = $state(false);

  let removing = $state<Zone | null>(null);
  let typed = $state("");

  const columns: Column[] = [
    { label: "Name" },
    { label: "Kind", width: "14rem" },
    { label: "Serial", align: "right", width: "7rem" },
    { label: "TTL", align: "right", width: "6rem" },
    { label: "Primary name server" },
    { label: "State", width: "8rem" },
    { label: "Changed", align: "right", width: "8rem" },
    { label: "", width: "5rem" },
  ];

  async function load() {
    loading = true;
    trouble = null;
    try {
      const answer = await api.get("/zones", {
        query: {
          limit: pageSize,
          cursor: cursors[pageIndex],
          ...(search.trim() ? { search: search.trim() } : {}),
          ...(kind ? { kind } : {}),
        },
      });
      zones = answer.items;
      nextCursor = answer.nextCursor;
      if (answer.nextCursor) cursors[pageIndex + 1] = answer.nextCursor;
    } catch (err) {
      zones = [];
      nextCursor = undefined;
      trouble =
        err instanceof NetworkError
          ? "The server did not answer."
          : err instanceof ApiError
            ? (err.detail ?? err.title)
            : "The zones could not be read.";
    } finally {
      loading = false;
    }
  }

  /** restart goes back to the first page, which every change to the query does. */
  function restart() {
    cursors = [undefined];
    pageIndex = 0;
  }

  // One request per pause rather than one per keystroke.
  let debounce: ReturnType<typeof setTimeout> | undefined;
  function onSearch(value: string) {
    search = value;
    clearTimeout(debounce);
    debounce = setTimeout(() => {
      restart();
      load();
    }, 200);
  }

  $effect(() => {
    // Once, on arrival. Everything that changes the query calls load itself,
    // and tracking the paging state here would make every one of those two
    // requests instead of one.
    untrack(() => load());
  });

  async function create(event: SubmitEvent) {
    event.preventDefault();
    working = true;
    refused = null;
    try {
      const ttl = newTtl.trim();
      const made = await api.post("/zones", {
        body: {
          name: newName.trim(),
          ...(ttl ? { defaultTtl: Number(ttl) } : {}),
        },
      });

      // A second commit rather than part of the first. Creating a zone and
      // putting a record in it are two changes, and the journal saying so is
      // what makes either of them revertible on its own.
      const address = newNsAddress.trim();
      if (address) {
        await api.post("/zones/{zoneId}/records", {
          path: { zoneId: made.id },
          body: {
            name: made.soa.primaryNs,
            type: address.includes(":") ? "AAAA" : "A",
            data: address,
          },
        });
      }

      creating = false;
      newName = "";
      newTtl = "";
      newNsAddress = "";
      await goto(`/zones/${encodeURIComponent(made.name)}`);
    } catch (err) {
      refused =
        err instanceof ApiError
          ? (err.detail ?? err.title)
          : "The zone could not be created.";
    } finally {
      working = false;
    }
  }

  async function remove() {
    if (!removing) return;
    working = true;
    refused = null;
    try {
      await api.delete("/zones/{zoneId}", { path: { zoneId: removing.id } });
      removing = null;
      typed = "";
      await load();
    } catch (err) {
      refused =
        err instanceof ApiError ? (err.detail ?? err.title) : "The zone could not be deleted.";
    } finally {
      working = false;
    }
  }

  function open(zone: Zone) {
    goto(`/zones/${encodeURIComponent(zone.name)}`);
  }
</script>

<svelte:head><title>Zones — Wegweiser</title></svelte:head>

<Bar title="Zones">
  {#snippet actions()}
    <label
      class="flex h-8 min-w-[18rem] items-center gap-2 rounded-sm border border-line
             bg-surface px-2.5 transition-colors focus-within:border-signal"
    >
      <svg
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2"
        class="size-3.5 shrink-0 text-ink-faint"
        aria-hidden="true"
      >
        <circle cx="11" cy="11" r="7" />
        <path d="m16.5 16.5 4 4" stroke-linecap="round" />
      </svg>
      <input
        value={search}
        oninput={(e) => onSearch(e.currentTarget.value)}
        placeholder="search every zone…"
        aria-label="Search zones"
        autocomplete="off"
        spellcheck="false"
        class="num min-w-0 flex-1 bg-transparent text-[12px] outline-none
               placeholder:text-ink-faint"
      />
      {#if search}
        <button
          type="button"
          onclick={() => onSearch("")}
          aria-label="Clear the search"
          class="grid size-4 cursor-pointer place-items-center rounded-xs text-ink-faint
                 hover:text-ink"
        >
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class="size-3">
            <path d="M6 6l12 12M18 6L6 18" stroke-linecap="round" />
          </svg>
        </button>
      {/if}
    </label>

    <label class="flex items-center gap-2">
      <span class="sr-only">Kind</span>
      <select
        value={kind}
        onchange={(e) => {
          kind = e.currentTarget.value as "" | "forward" | "reverse";
          restart();
          load();
        }}
        class="num h-8 cursor-pointer rounded-sm border border-line bg-surface px-2 text-[12px]
               text-ink outline-none focus:border-signal"
      >
        <option value="">any kind</option>
        <option value="forward">forward</option>
        <option value="reverse">reverse</option>
      </select>
    </label>

    {#if session.can("write")}
      <Button weight="primary" onclick={() => ((creating = true), (refused = null))}>
        + New zone
      </Button>
    {/if}
  {/snippet}
</Bar>

<div class="flex-1 overflow-auto">
  {#if trouble}
    <div class="px-5 pt-4">
      <Notice tone="crit" title="The zones could not be listed">
        {trouble}
        {#snippet actions()}
          <Button onclick={load}>Try again</Button>
        {/snippet}
      </Notice>
    </div>
  {/if}

  <Table {columns} items={zones} key={(z) => z.id}>
  {#snippet row(zone: Zone)}
    <td class="relative py-1.5 pr-3 pl-5">
      {#if zone.disabled}
        <span class="absolute top-0 bottom-0 left-0 w-0.5 bg-ink-faint"></span>
      {/if}
      <button
        type="button"
        onclick={() => open(zone)}
        class="num cursor-pointer text-left text-[13px] hover:text-signal
               {zone.disabled ? 'text-ink-faint' : ''}"
      >
        {zone.name}
      </button>
    </td>

    <td class="px-3 py-1.5">
      {#if zone.kind === "reverse"}
        <span class="flex items-center gap-2">
          <Chip tone="signal">Reverse</Chip>
          <span class="num text-[11px] text-ink-faint">{zone.prefix ?? ""}</span>
        </span>
      {:else}
        <Chip>Forward</Chip>
      {/if}
    </td>

    <td class="num px-3 py-1.5 text-right">{zone.soa.serial}</td>
    <td class="num px-3 py-1.5 text-right text-ink-mute">{zone.defaultTtl}</td>
    <td class="num px-3 py-1.5 text-ink-mute">{zone.soa.primaryNs}</td>

    <td class="px-3 py-1.5">
      {#if zone.disabled}
        <Chip dot>Disabled</Chip>
      {:else}
        <Chip tone="ok" dot>Serving</Chip>
      {/if}
    </td>

    <td class="num px-3 py-1.5 text-right text-[12px] text-ink-faint" title={exact(zone.updatedAt)}>
      {ago(zone.updatedAt)}
    </td>

    <td class="py-1.5 pr-5 pl-3 text-right">
      {#if session.can("write")}
        <button
          type="button"
          onclick={() => ((removing = zone), (typed = ""), (refused = null))}
          aria-label="Delete {zone.name}"
          title="Delete {zone.name}"
          class="grid size-6 cursor-pointer place-items-center rounded-xs text-ink-faint
                 opacity-0 transition-colors group-hover:opacity-100 hover:bg-crit-lo
                 hover:text-crit focus-visible:opacity-100"
        >
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" class="size-3.5">
            <path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13" />
          </svg>
        </button>
      {/if}
    </td>
  {/snippet}

  {#snippet empty()}
    {#if loading}
      <p class="text-center text-[13px] text-ink-faint">Reading the zones…</p>
    {:else if search.trim() || kind}
      <Empty title="Nothing matches">
        No zone on this server matches what is being asked for.
        {#snippet actions()}
          <Button
            onclick={() => {
              kind = "";
              onSearch("");
            }}
          >
            Clear the filters
          </Button>
        {/snippet}
      </Empty>
    {:else}
      <Empty title="No zones yet">
        A zone is a name this server is authoritative for. Create one and it starts answering
        immediately; there is no reload and no zonefile to write.
        {#snippet actions()}
          {#if session.can("write")}
            <Button weight="primary" onclick={() => (creating = true)}>+ New zone</Button>
          {/if}
        {/snippet}
      </Empty>
    {/if}
    {/snippet}
  </Table>
</div>

<Pager
  page={pageIndex}
  size={pageSize}
  shown={zones.length}
  hasNext={nextCursor !== undefined}
  busy={loading}
  onchange={({ page, size }) => {
    if (size !== pageSize) {
      pageSize = size;
      restart();
    } else {
      pageIndex = page;
    }
    load();
  }}
/>

<!-- Create ------------------------------------------------------------- -->
<Dialog bind:open={creating} title="New zone">
  <form id="create-zone" class="flex flex-col gap-4" onsubmit={create}>
    <Field
      label="Name"
      bind:value={newName}
      placeholder="example.com or 192.168.0.0/16"
      autocomplete="off"
      spellcheck={false}
      hint="A domain name for a forward zone, a trailing dot is optional and changes nothing. Or a network, which becomes the reverse zone that answers for it: 192.168.0.0/16 is 168.192.in-addr.arpa."
    />
    <Field
      label="Default TTL"
      bind:value={newTtl}
      placeholder="3600"
      inputmode="numeric"
      hint="Applied to a record added without one. Left empty, this server's own default is used."
    />
    <Field
      label="Address of the name server"
      bind:value={newNsAddress}
      placeholder="192.0.2.10"
      autocomplete="off"
      spellcheck={false}
      hint="Optional, and nothing is invented for it: this server does not know which of its addresses the world reaches it on. Without one, the zone answers NXDOMAIN for its own name server and a delegation to it is lame."
    />
    {#if refused}
      <p
        class="rounded-sm border border-line border-l-2 border-l-crit bg-raised px-3 py-2
               text-[13px] text-ink-mute"
        role="alert"
      >
        {refused}
      </p>
    {/if}
  </form>

  {#snippet actions()}
    <Button weight="quiet" onclick={() => (creating = false)}>Cancel</Button>
    <Button weight="primary" type="submit" form="create-zone" disabled={working || !newName.trim()}>
      {working ? "Creating…" : "Create zone"}
    </Button>
  {/snippet}
</Dialog>

<!-- Delete ------------------------------------------------------------- -->
<Dialog
  open={removing !== null}
  onclose={() => (removing = null)}
  title="Delete {removing?.name ?? ''}"
>
  <p class="text-[13px] text-ink-mute">
    Every record in this zone goes with it and the server stops answering for the name. The
    history is kept, a commit outlives the zone it describes, but the zone itself is not
    coming back.
  </p>
  <Field
    label="Type the name to confirm"
    bind:value={typed}
    autocomplete="off"
    spellcheck={false}
    placeholder={removing?.name ?? ""}
  />
  {#if refused}
    <p
      class="rounded-sm border border-line border-l-2 border-l-crit bg-raised px-3 py-2
             text-[13px] text-ink-mute"
      role="alert"
    >
      {refused}
    </p>
  {/if}

  {#snippet actions()}
    <Button weight="quiet" onclick={() => (removing = null)}>Cancel</Button>
    <Button
      weight="primary"
      onclick={remove}
      disabled={working || typed.trim() !== removing?.name}
      class="border-crit bg-crit text-ground hover:border-crit hover:bg-crit"
    >
      {working ? "Deleting…" : "Delete zone"}
    </Button>
  {/snippet}
</Dialog>
