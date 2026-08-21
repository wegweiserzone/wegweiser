<script lang="ts">
  /**
   * Everything that has ever been written.
   */
  import { untrack } from "svelte";

  import { page } from "$app/state";
  import { replaceState } from "$app/navigation";
  import { api, ApiError, NetworkError } from "$lib/api";
  import type { Commit, CommitKind, Conflict, MissingZone, Zone } from "$lib/api";
  import { ago, exact } from "$lib/format";
  import { session } from "$lib/session.svelte";
  import Bar from "$lib/components/Bar.svelte";
  import Button from "$lib/components/Button.svelte";
  import Chip from "$lib/components/Chip.svelte";
  import Diff from "$lib/components/Diff.svelte";
  import Dialog from "$lib/components/Dialog.svelte";
  import Empty from "$lib/components/Empty.svelte";
  import Notice from "$lib/components/Notice.svelte";
  import Pager from "$lib/components/Pager.svelte";
  import RecordWritten from "$lib/components/RecordWritten.svelte";

  // Typed against the spec rather than as strings, so a kind renamed there is
  // a compile error here instead of a filter that silently matches nothing.
  const kinds: CommitKind[] = [
    "edit",
    "zone_create",
    "zone_update",
    "zone_delete",
    "import",
    "rollback",
  ];

  const kindLabel: Record<string, string> = {
    edit: "Edit",
    zone_create: "Created",
    zone_update: "Settings",
    zone_delete: "Deleted",
    import: "Import",
    rollback: "Rollback",
  };
  const kindTone: Record<string, "neutral" | "ok" | "warn" | "crit" | "info" | "signal"> = {
    edit: "neutral",
    zone_create: "ok",
    zone_update: "neutral",
    zone_delete: "crit",
    import: "info",
    rollback: "warn",
  };

  let commits = $state<Commit[]>([]);
  let zones = $state<Zone[]>([]);
  let loading = $state(true);
  let trouble = $state<string | null>(null);

  let zoneFilter = $state(page.url.searchParams.get("zone") ?? "");
  let kindFilter = $state<CommitKind | "">("");
  let pageIndex = $state(0);
  let pageSize = $state(250);
  let cursors = $state<(string | undefined)[]>([undefined]);
  let nextCursor = $state<string | undefined>(undefined);

  let selected = $state<Commit | null>(null);
  let detail = $state<Commit | null>(null);
  let detailTrouble = $state<string | null>(null);

  let reverting = $state<Commit | null>(null);
  let working = $state(false);
  let refused = $state<string | null>(null);
  let reverted = $state<{
    commit?: Commit;
    conflicts: Conflict[];
    missingZones: MissingZone[];
  } | null>(null);

  const writable = $derived(session.can("write"));
  const chosenZone = $derived(zones.find((z) => z.name === zoneFilter));

  async function loadZones() {
    try {
      const answer = await api.get("/zones", { query: { limit: 1000 } });
      zones = answer.items;
    } catch {
      zones = [];
    }
  }

  async function load() {
    loading = true;
    trouble = null;
    try {
      const answer = await api.get("/commits", {
        query: {
          limit: pageSize,
          cursor: cursors[pageIndex],
          ...(chosenZone ? { zoneId: chosenZone.id } : {}),
          ...(kindFilter ? { kind: [kindFilter] } : {}),
        },
      });
      commits = answer.items;
      nextCursor = answer.nextCursor;
      if (answer.nextCursor) cursors[pageIndex + 1] = answer.nextCursor;

      // The first commit is the one you almost always want to look at, and
      // after a revert it is the revert, so its diff is what just happened.
      if (!selected && commits[0]) show(commits[0], false);
    } catch (err) {
      commits = [];
      nextCursor = undefined;
      trouble =
        err instanceof NetworkError
          ? "The server did not answer."
          : err instanceof ApiError
            ? (err.detail ?? err.title)
            : "The history could not be read.";
    } finally {
      loading = false;
    }
  }

  function restart() {
    cursors = [undefined];
    pageIndex = 0;
  }

  /** choose is somebody picking a commit, which puts the last result away. */
  function choose(commit: Commit) {
    return show(commit, true);
  }

  /**
   * show reads one commit in full, because a listing leaves its events out.
   *
   * `clear` is what separates a person clicking from the list choosing for
   * them: the result of a revert is about the zone, not about whichever commit
   * happens to be selected, and reloading the list right after one must not
   * take the answer off the screen before it has been read.
   */
  async function show(commit: Commit, clear: boolean) {
    selected = commit;
    detail = null;
    detailTrouble = null;
    if (clear) reverted = null;
    try {
      detail = await api.get("/commits/{commitId}", { path: { commitId: commit.id } });
    } catch (err) {
      detailTrouble =
        err instanceof ApiError
          ? (err.detail ?? err.title)
          : "The changes this commit made could not be read.";
    }
  }

  $effect(() => {
    // The zones first, and waited for: the zone filter is a name in the URL
    // and the API takes an identifier, so arriving at /history?zone=… cannot
    // filter anything until the name has been resolved. Firing both at once
    // meant a link from a zone landed on the unfiltered history.
    untrack(async () => {
      await loadZones();
      await load();
    });
  });

  function applyFilters() {
    restart();
    selected = null;
    detail = null;
    // The zone is in the URL so that a link to this view carries it.
    const url = new URL(page.url);
    if (zoneFilter) url.searchParams.set("zone", zoneFilter);
    else url.searchParams.delete("zone");
    replaceState(url, page.state);
    load();
  }

  async function revert() {
    if (!reverting) return;
    const target = zones.find((z) => z.id === reverting?.zoneId);
    if (!target) {
      refused = "That zone no longer exists, so there is nothing to put back.";
      return;
    }
    working = true;
    refused = null;
    try {
      const result = await api.post("/zones/{zoneId}/rollback", {
        path: { zoneId: target.id },
        body: { serial: reverting.serialTo, comment: `revert to serial ${reverting.serialTo}` },
      });
      reverted = {
        commit: result.commit,
        conflicts: result.conflicts ?? [],
        missingZones: result.missingZones ?? [],
      };
      reverting = null;
      restart();
      selected = null;
      await load();
    } catch (err) {
      refused =
        err instanceof ApiError
          ? (err.detail ?? err.title)
          : "The zone could not be put back.";
    } finally {
      working = false;
    }
  }
</script>

<svelte:head><title>History — Wegweiser</title></svelte:head>

<Bar title="History">
  {#snippet actions()}
    <label class="flex items-center gap-2">
      <span class="sr-only">Zone</span>
      <select
        value={zoneFilter}
        onchange={(e) => {
          zoneFilter = e.currentTarget.value;
          applyFilters();
        }}
        class="num h-8 max-w-[16rem] cursor-pointer rounded-sm border border-line bg-surface
               px-2 text-[12px] text-ink outline-none focus:border-signal"
      >
        <option value="">every zone</option>
        {#each zones as zone (zone.id)}
          <option value={zone.name}>{zone.name}</option>
        {/each}
      </select>
    </label>

    <label class="flex items-center gap-2">
      <span class="sr-only">Kind</span>
      <select
        value={kindFilter}
        onchange={(e) => {
          kindFilter = e.currentTarget.value as CommitKind | "";
          applyFilters();
        }}
        class="num h-8 cursor-pointer rounded-sm border border-line bg-surface px-2 text-[12px]
               text-ink outline-none focus:border-signal"
      >
        <option value="">any change</option>
        {#each kinds as kind (kind)}
          <option value={kind}>{kindLabel[kind]}</option>
        {/each}
      </select>
    </label>
  {/snippet}
</Bar>

<div class="grid min-h-0 flex-1 grid-cols-1 lg:grid-cols-[minmax(22rem,2fr)_minmax(0,3fr)]">
  <!-- The commits -->
  <div class="flex min-h-0 flex-col border-line lg:border-r">
    <div class="flex flex-1 flex-col overflow-auto">
      {#if trouble}
        <div class="p-4">
          <Notice tone="crit" title="The history could not be read">
            {trouble}
            {#snippet actions()}
              <Button onclick={load}>Try again</Button>
            {/snippet}
          </Notice>
        </div>
      {:else if loading && commits.length === 0}
        <p class="px-5 py-16 text-center text-[13px] text-ink-faint">Reading the history…</p>
      {:else if commits.length === 0}
        <div class="px-5 py-16">
          <Empty title="Nothing has been written">
            Every change to this server is a commit, and there are none yet. Create a zone and
            it appears here, with the records it started with.
          </Empty>
        </div>
      {:else}
        {#each commits as commit (commit.id)}
          <button
            type="button"
            onclick={() => choose(commit)}
            aria-current={selected?.id === commit.id ? "true" : undefined}
            class="group relative grid cursor-pointer grid-cols-[5.5rem_minmax(0,1fr)_auto]
                   items-center gap-3 border-b border-line-soft px-5 py-2.5 text-left
                   transition-colors hover:bg-surface aria-[current]:bg-raised"
          >
            <span
              class="absolute top-0 bottom-0 left-0 w-0.5 bg-signal opacity-0
                     group-aria-[current]:opacity-100"
            ></span>

            <span class="num text-[12px] text-ink-faint">
              {commit.serialFrom}<span class="px-1">→</span><span class="text-ink"
                >{commit.serialTo}</span
              >
            </span>

            <span class="flex min-w-0 flex-col gap-0.5">
              <span class="flex items-center gap-2">
                <Chip tone={kindTone[commit.kind] ?? "neutral"}>
                  {kindLabel[commit.kind] ?? commit.kind}
                </Chip>
                <span class="num truncate text-[12px]">{commit.zoneName}</span>
              </span>
              <span class="num truncate text-[11px] text-ink-faint">
                {commit.source}{commit.actor ? ` · ${commit.actor}` : ""}{commit.comment
                  ? ` · ${commit.comment}`
                  : ""}
              </span>
            </span>

            <span class="num text-[11px] whitespace-nowrap text-ink-faint" title={exact(commit.createdAt)}>
              {ago(commit.createdAt)}
            </span>
          </button>
        {/each}
      {/if}
    </div>

    <Pager
      page={pageIndex}
      size={pageSize}
      shown={commits.length}
      hasNext={nextCursor !== undefined}
      busy={loading}
      onchange={({ page: next, size }) => {
        if (size !== pageSize) {
          pageSize = size;
          restart();
        } else {
          pageIndex = next;
        }
        load();
      }}
    />
  </div>

  <!-- What it changed -->
  <div class="flex min-h-0 flex-col">
    {#if !selected}
      <div class="flex flex-1 items-center justify-center px-5 py-16">
        <Empty title="Nothing chosen">Pick a commit to see what it changed.</Empty>
      </div>
    {:else}
      <div class="flex shrink-0 items-center gap-3 border-b border-line bg-surface px-5 py-2.5">
        <span class="num text-[13px]">
          {selected.serialFrom}<span class="px-1 text-ink-faint">→</span>{selected.serialTo}
        </span>
        <span class="num truncate text-[12px] text-ink-mute">{selected.zoneName}</span>
        {#if selected.revertsTo !== undefined}
          <Chip tone="warn">restored {selected.revertsTo}</Chip>
        {/if}

        {#if writable && selected.kind !== "zone_delete"}
          <Button
            class="ml-auto"
            onclick={() => ((reverting = selected), (refused = null))}
          >
            Revert to this state
          </Button>
        {/if}
      </div>

      <div class="flex flex-1 flex-col gap-4 overflow-auto py-4">
        {#if reverted}
          <div class="px-5">
            <Notice tone="signal" title={reverted.commit ? "The zone is back at that state" : "Nothing to do"}>
              {#if reverted.commit}
                Written forward as serial {reverted.commit.serialTo}, not rewound: a secondary
                that has already seen a higher serial would never accept a jump back to a lower
                one (RFC 1982).
              {:else}
                The zone was already at that serial, so nothing was written.
              {/if}
            </Notice>
          </div>
          {#if reverted.conflicts.length > 0 || reverted.missingZones.length > 0}
            <div class="px-5">
              <RecordWritten
                conflicts={reverted.conflicts}
                missingZones={reverted.missingZones}
              />
            </div>
          {/if}
        {/if}

        {#if detailTrouble}
          <div class="px-5">
            <Notice tone="crit" title="The changes could not be read">{detailTrouble}</Notice>
          </div>
        {:else if !detail}
          <p class="px-5 text-[13px] text-ink-faint">Reading the changes…</p>
        {:else if (detail.events ?? []).length === 0}
          <div class="px-5 py-12">
            <Empty title="No records changed">
              This commit changed the zone itself rather than what is in it; its settings, or
              its start of authority.
            </Empty>
          </div>
        {:else}
          <Diff events={detail.events ?? []} />
        {/if}
      </div>
    {/if}
  </div>
</div>

<Dialog
  open={reverting !== null}
  onclose={() => (reverting = null)}
  title="Revert to serial {reverting?.serialTo ?? ''}"
>
  <p class="text-[13px] text-ink-mute">
    <span class="num text-ink">{reverting?.zoneName}</span> is put back to the state it was in
    after this commit. The difference is written as a <em>new</em> commit moving forward; it
    is not a rewind, because a secondary that has already seen a higher serial would never
    accept a jump back to a lower one.
  </p>
  <p class="text-[13px] text-ink-mute">
    Records that did not change in between keep their identity, their comments and the history
    pointing at them. Records this server generated follow the records they came from, so
    restoring those is what moves them.
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
    <Button weight="quiet" onclick={() => (reverting = null)}>Cancel</Button>
    <Button weight="primary" onclick={revert} disabled={working}>
      {working ? "Reverting…" : "Revert the zone"}
    </Button>
  {/snippet}
</Dialog>
