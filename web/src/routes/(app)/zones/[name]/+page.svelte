<script lang="ts">
  /**
   * The records of one zone.
   *
   * A record this server generated cannot be edited in place; it follows the
   * record it came from, so the edit offers to detach it instead, which is
   * exactly what D4 says the way out is.
   */
  import { untrack } from "svelte";

  import { invalidateAll } from "$app/navigation";
  import { api, ApiError, NetworkError } from "$lib/api";
  import type { Conflict, MissingZone, Record_ } from "$lib/api";
  import { relative } from "$lib/format";
  import { lameNameServers } from "$lib/health";
  import type { LameNS } from "$lib/health";
  import { everyType } from "$lib/records";
  import { session } from "$lib/session.svelte";
  import Button from "$lib/components/Button.svelte";
  import Chip from "$lib/components/Chip.svelte";
  import Dialog from "$lib/components/Dialog.svelte";
  import Empty from "$lib/components/Empty.svelte";
  import Field from "$lib/components/Field.svelte";
  import Notice from "$lib/components/Notice.svelte";
  import Pager from "$lib/components/Pager.svelte";
  import RecordWritten from "$lib/components/RecordWritten.svelte";
  import RData from "$lib/components/RData.svelte";
  import Table from "$lib/components/Table.svelte";
  import type { Column } from "$lib/components/Table.svelte";
  import TypeField from "$lib/components/TypeField.svelte";

  let { data } = $props();
  const zone = $derived(data.zone);
  const writable = $derived(session.can("write"));

  /**
   * The listing is the server's, not this page's.
   */
  let records = $state<Record_[]>([]);
  let loading = $state(true);
  let trouble = $state<string | null>(null);

  let search = $state("");
  let typeFilter = $state("");

  /** Name servers this zone points at and has no address for (RFC 1912 §2.8). */
  let lame = $state<LameNS[]>([]);
  let pageIndex = $state(0);
  let pageSize = $state(250);
  /** The cursor that opens each page. The first page has none. */
  let cursors = $state<(string | undefined)[]>([undefined]);
  let nextCursor = $state<string | undefined>(undefined);

  // What the last write caused, kept until the next one.
  let written = $state<{
    generated: Record_[];
    conflicts: Conflict[];
    missingZones: MissingZone[];
  } | null>(null);

  const columns: Column[] = [
    { label: "Name" },
    { label: "Type", width: "7rem" },
    { label: "TTL", align: "right", width: "6rem" },
    { label: "Data" },
    { label: "Origin", width: "9rem" },
    { label: "", width: "6rem" },
  ];

  /** managed reports a record this server writes and maintains. */
  function managed(r: Record_): boolean {
    return r.managedBy !== undefined || r.managedKind !== undefined;
  }

  async function load() {
    loading = true;
    trouble = null;
    try {
      const answer = await api.get("/zones/{zoneId}/records", {
        path: { zoneId: zone.id },
        query: {
          limit: pageSize,
          cursor: cursors[pageIndex],
          ...(search.trim() ? { search: search.trim() } : {}),
          ...(typeFilter ? { type: typeFilter } : {}),
        },
      });
      records = answer.items;
      nextCursor = answer.nextCursor;
      // Remember what opens the page after this one, so Next can go there and
      // Previous can come back without re-walking from the start.
      if (answer.nextCursor) cursors[pageIndex + 1] = answer.nextCursor;
    } catch (err) {
      records = [];
      nextCursor = undefined;
      trouble =
        err instanceof NetworkError
          ? "The server did not answer."
          : err instanceof ApiError
            ? (err.detail ?? err.title)
            : "The records could not be read.";
    } finally {
      loading = false;
    }
  }

  /** checkGlue looks for the one defect a zone can have that looks like health. */
  async function checkGlue() {
    try {
      lame = await lameNameServers(zone);
    } catch {
      // A diagnosis that cannot be made is not a failure worth a banner: the
      // records themselves are what this page is for.
      lame = [];
    }
  }

  /** restart goes back to the first page, which every change to the query does. */
  function restart() {
    cursors = [undefined];
    pageIndex = 0;
  }

  // Typing narrows server-side, so it is one request per pause rather than one
  // per keystroke.
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
    // The zone's identity is the only thing this reacts to. Everything inside
    // is untracked because load reads the paging state and restart writes it:
    // an effect that depends on what it changes re-runs itself for ever, and
    // the table sits on "Reading the records…" while it does.
    void zone.id;
    untrack(() => {
      restart();
      load();
      checkGlue();
    });
  });

  async function afterWrite(result: {
    generated?: Record_[];
    conflicts?: Conflict[];
    missingZones?: MissingZone[];
  }) {
    written = {
      generated: result.generated ?? [],
      conflicts: result.conflicts ?? [],
      missingZones: result.missingZones ?? [],
    };
    restart();
    await load();
    await checkGlue();
    // The serial moved, and the strip above shows it.
    await invalidateAll();
  }

  function why(err: unknown, fallback: string): string {
    if (err instanceof ApiError) return err.detail ?? err.title;
    if (err instanceof NetworkError) return "The server did not answer.";
    return fallback;
  }

  /* ── Adding ───────────────────────────────────────────────────────────── */

  let adding = $state(false);
  let newName = $state("");
  let newType = $state("A");
  let newTtl = $state("");
  let newData = $state("");
  let refused = $state<string | null>(null);
  let working = $state(false);

  /** qualify completes a relative name against the apex, the way a zonefile does. */
  function qualify(name: string): string {
    const typed = name.trim();
    if (typed === "" || typed === "@") return zone.name;
    if (typed.endsWith(".")) return typed;
    return `${typed}.${zone.name}`;
  }

  const commonTypes = ["A", "AAAA", "CNAME", "MX", "TXT", "NS", "SRV", "CAA", "PTR"];

  async function add(event: SubmitEvent) {
    event.preventDefault();
    working = true;
    refused = null;
    try {
      const ttl = newTtl.trim();
      const result = await api.post("/zones/{zoneId}/records", {
        path: { zoneId: zone.id },
        body: {
          name: qualify(newName),
          type: newType.trim().toUpperCase(),
          data: newData.trim(),
          ...(ttl ? { ttl: Number(ttl) } : {}),
        },
      });
      adding = false;
      newName = "";
      newData = "";
      newTtl = "";
      await afterWrite(result);
    } catch (err) {
      refused = why(err, "The record could not be added.");
    } finally {
      working = false;
    }
  }

  /* ── Editing ──────────────────────────────────────────────────────────── */

  let editing = $state<Record_ | null>(null);
  let editTtl = $state("");
  let editData = $state("");
  /** The record a generated one follows, written out rather than identified. */
  let source = $state<string | null>(null);

  async function startEdit(record: Record_) {
    editing = record;
    editTtl = String(record.ttl);
    editData = record.data;
    refused = null;
    source = null;

    // A ULID tells a person nothing. One request turns it into the record
    // they would have to change to change this one.
    if (record.managedBy) {
      try {
        const from = await api.get("/records/{recordId}", {
          path: { recordId: record.managedBy },
        });
        source = `${from.name} ${from.ttl} IN ${from.type} ${from.data}`;
      } catch {
        // It may be gone, or in a zone this token cannot read. The dialog
        // still works; it just cannot name what it came from.
      }
    }
  }

  async function saveEdit(event: SubmitEvent) {
    event.preventDefault();
    if (!editing) return;
    working = true;
    refused = null;
    try {
      const result = await api.patch("/records/{recordId}", {
        path: { recordId: editing.id },
        body: { ttl: Number(editTtl.trim()), data: editData.trim() },
      });
      editing = null;
      await afterWrite(result);
    } catch (err) {
      refused = why(err, "The record could not be changed.");
    } finally {
      working = false;
    }
  }

  async function detach() {
    if (!editing) return;
    working = true;
    refused = null;
    try {
      const result = await api.post("/records/{recordId}/detach", {
        path: { recordId: editing.id },
      });
      editing = result.record;
      await afterWrite(result);
    } catch (err) {
      refused = why(err, "The record could not be taken over.");
    } finally {
      working = false;
    }
  }

  /* ── Removing ─────────────────────────────────────────────────────────── */

  let removing = $state<Record_ | null>(null);

  async function remove() {
    if (!removing) return;
    working = true;
    refused = null;
    try {
      await api.delete("/records/{recordId}", { path: { recordId: removing.id } });
      removing = null;
      written = null;
      await load();
      await invalidateAll();
    } catch (err) {
      refused = why(err, "The record could not be deleted.");
    } finally {
      working = false;
    }
  }

  /** The type's colour says what kind of thing it is before the letters do. */
  const typeInk: Record<string, string> = {
    A: "text-info",
    AAAA: "text-info",
    PTR: "text-signal",
    CNAME: "text-warn",
    SOA: "text-ok",
    NS: "text-ok",
  };

  /** Rows repeat the owner name only when it changes, the way a zonefile does. */
  function repeats(index: number): boolean {
    return index > 0 && records[index - 1]?.name === records[index]?.name;
  }
</script>

<div class="flex shrink-0 items-center gap-2 border-b border-line px-5 py-2.5">
  <label
    class="flex h-8 min-w-[20rem] items-center gap-2 rounded-sm border border-line bg-surface
           px-2.5 transition-colors focus-within:border-signal"
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
      placeholder="search names and data…"
      aria-label="Search records"
      autocomplete="off"
      spellcheck="false"
      class="num min-w-0 flex-1 bg-transparent text-[12px] outline-none placeholder:text-ink-faint"
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
    <span class="sr-only">Filter by type</span>
    <select
      value={typeFilter}
      onchange={(e) => {
        typeFilter = e.currentTarget.value;
        restart();
        load();
      }}
      class="num h-8 cursor-pointer rounded-sm border border-line bg-surface px-2 text-[12px]
             text-ink outline-none focus:border-signal"
    >
      <option value="">any type</option>
      {#each everyType as type (type)}
        <option value={type}>{type}</option>
      {/each}
    </select>
  </label>

  {#if writable}
    <Button
      weight="primary"
      class="ml-auto"
      onclick={() => ((adding = true), (refused = null), (newName = ""), (newData = ""))}
    >
      + New record
    </Button>
  {/if}
</div>

<div class="flex flex-1 flex-col overflow-auto">
  {#if trouble}
    <div class="px-5 pt-4">
      <Notice tone="crit" title="The records could not be listed">
        {trouble}
        {#snippet actions()}
          <Button onclick={load}>Try again</Button>
        {/snippet}
      </Notice>
    </div>
  {/if}

  {#each lame as server (server.target)}
    <div class="px-5 pt-4">
      <Notice tone="warn" title="A name server here has no address">
        <code class="num text-ink">{server.target}</code> answers for
        <code class="num text-ink">{server.owner}</code>, and this zone holds no A or AAAA
        for it, so a resolver referred here is told, authoritatively, that the name does not
        exist. Give it an address, or point the delegation at a name server outside this
        zone.
        {#snippet actions()}
          {#if writable}
            <Button
              onclick={() => {
                adding = true;
                refused = null;
                newName = relative(server.target, zone.name);
                newType = "A";
                newData = "";
              }}
            >
              Add its address
            </Button>
          {/if}
        {/snippet}
      </Notice>
    </div>
  {/each}

  {#if written}
    <div class="px-5 pt-4">
      <RecordWritten
        generated={written.generated}
        conflicts={written.conflicts}
        missingZones={written.missingZones}
      />
    </div>
  {/if}

  <Table {columns} items={records} key={(r) => r.id}>
    {#snippet row(record: Record_)}
      {@const index = records.indexOf(record)}
      <td class="num py-1.5 pr-3 pl-5 text-[13px]">
        {#if repeats(index)}
          <span class="text-ink-faint" title={record.name}>⌐</span>
        {:else}
          <span class={record.name === zone.name ? "text-ink-faint" : ""}>
            {relative(record.name, zone.name)}
          </span>
        {/if}
      </td>

      <td class="num px-3 py-1.5 text-[12px] font-medium {typeInk[record.type] ?? ''}">
        {record.type}
      </td>

      <td
        class="num px-3 py-1.5 text-right {record.ttl === zone.defaultTtl
          ? 'text-ink-faint'
          : 'text-ink'}"
      >
        {record.ttl}
      </td>

      <td class="num max-w-[44ch] truncate px-3 py-1.5" title={record.data}>
        {record.data}
      </td>

      <td class="px-3 py-1.5">
        {#if managed(record)}
          <Chip tone="signal" title="Written and maintained by this server">generated</Chip>
        {:else if record.type === "SOA" || record.type === "NS"}
          <Chip>zone</Chip>
        {:else}
          <span class="text-[12px] text-ink-faint">—</span>
        {/if}
      </td>

      <td class="py-1.5 pr-5 pl-3 text-right">
        {#if writable && record.type !== "SOA"}
          <div class="flex justify-end gap-0.5 opacity-0 transition-opacity group-hover:opacity-100 group-focus-within:opacity-100">
            <button
              type="button"
              onclick={() => startEdit(record)}
              aria-label="Edit {record.name} {record.type}"
              class="grid size-6 cursor-pointer place-items-center rounded-xs text-ink-faint
                     transition-colors hover:bg-raised hover:text-ink"
            >
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" class="size-3.5">
                <path d="M4 20h4L19 9l-4-4L4 16v4Z" />
              </svg>
            </button>
            <button
              type="button"
              onclick={() => ((removing = record), (refused = null))}
              aria-label="Delete {record.name} {record.type}"
              class="grid size-6 cursor-pointer place-items-center rounded-xs text-ink-faint
                     transition-colors hover:bg-crit-lo hover:text-crit"
            >
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" class="size-3.5">
                <path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13" />
              </svg>
            </button>
          </div>
        {/if}
      </td>
    {/snippet}

    {#snippet empty()}
      {#if loading}
        <p class="text-center text-[13px] text-ink-faint">Reading the records…</p>
      {:else if search.trim() || typeFilter}
        <Empty title="Nothing matches">
          Nothing in this zone
          {#if search.trim()}contains <span class="num text-ink">{search}</span>{/if}{#if search.trim() && typeFilter}
            and
          {/if}{#if typeFilter}is a <span class="num text-ink">{typeFilter}</span> record{/if}.
          {#snippet actions()}
            <Button
              onclick={() => {
                typeFilter = "";
                onSearch("");
              }}
            >
              Clear the filters
            </Button>
          {/snippet}
        </Empty>
      {:else}
        <Empty title="An empty zone">
          This zone has its start of authority and its name server and nothing else. Add an
          address record and, if this server holds the reverse zone, the matching PTR is
          written with it.
          {#snippet actions()}
            {#if writable}
              <Button weight="primary" onclick={() => (adding = true)}>+ New record</Button>
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
  shown={records.length}
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

<!-- Add ---------------------------------------------------------------- -->
<Dialog bind:open={adding} title="New record">
  <form id="add-record" class="flex flex-col gap-4" onsubmit={add}>
    <div class="grid grid-cols-[minmax(0,1fr)_10rem] gap-3">
      <Field
        label="Name"
        bind:value={newName}
        placeholder="www"
        autocomplete="off"
        spellcheck={false}
        hint="Relative to the zone, empty or @ is the apex itself."
      />
      <TypeField bind:value={newType} />
    </div>
    <RData type={newType} bind:value={newData} />
    <Field
      label="TTL"
      bind:value={newTtl}
      placeholder={String(zone.defaultTtl)}
      inputmode="numeric"
      hint="Left empty, the zone's default is used."
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
    <Button weight="quiet" onclick={() => (adding = false)}>Cancel</Button>
    <Button
      weight="primary"
      type="submit"
      form="add-record"
      disabled={working || !newData.trim() || !newType.trim()}
    >
      {working ? "Adding…" : "Add record"}
    </Button>
  {/snippet}
</Dialog>

<!-- Edit --------------------------------------------------------------- -->
<Dialog
  open={editing !== null}
  onclose={() => (editing = null)}
  title="Edit {editing ? relative(editing.name, zone.name) : ''} {editing?.type ?? ''}"
>
  {#if editing && managed(editing)}
    <Notice tone="signal" title="This server maintains this record">
      {#if source}
        It follows
        <code class="num text-ink">{source}</code>, so editing it here is refused: the next
        time that record is written, this one would be written over.
      {:else}
        It was written by this server and follows the record it came from, so editing it here
        is refused.
      {/if}
      Take it over and it stops following: from then on it is yours to change and yours to
      keep correct.
    </Notice>
  {/if}

  <form id="edit-record" class="flex flex-col gap-4" onsubmit={saveEdit}>
    <RData
      type={editing?.type ?? ""}
      bind:value={editData}
      disabled={editing !== null && managed(editing)}
    />
    <Field
      label="TTL"
      bind:value={editTtl}
      inputmode="numeric"
      disabled={editing !== null && managed(editing)}
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
    <Button weight="quiet" onclick={() => (editing = null)}>Cancel</Button>
    {#if editing && managed(editing)}
      <Button weight="primary" onclick={detach} disabled={working}>
        {working ? "Taking over…" : "Take it over"}
      </Button>
    {:else}
      <Button weight="primary" type="submit" form="edit-record" disabled={working}>
        {working ? "Saving…" : "Save"}
      </Button>
    {/if}
  {/snippet}
</Dialog>

<!-- Delete ------------------------------------------------------------- -->
<Dialog
  open={removing !== null}
  onclose={() => (removing = null)}
  title="Delete this record"
>
  <p class="num rounded-sm border border-line bg-raised px-3 py-2 text-[12px]">
    {removing?.name}
    {removing?.ttl} IN {removing?.type}
    {removing?.data}
  </p>
  <p class="text-[13px] text-ink-mute">
    {#if removing && managed(removing)}
      This record was generated. Deleting it here removes it now, and it comes back the next
      time the record it follows is written.
    {:else}
      Anything this server generated from it goes too. The history keeps both, so
      <code class="num text-ink">weg zone rollback</code> can put them back.
    {/if}
  </p>

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
      disabled={working}
      class="border-crit bg-crit text-ground hover:border-crit hover:bg-crit"
    >
      {working ? "Deleting…" : "Delete record"}
    </Button>
  {/snippet}
</Dialog>
