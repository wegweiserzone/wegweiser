<script lang="ts">
  /**
   * The servers this one keeps its zones in step with.
   *
   * What a server knows is its own copy of the cluster's configuration, so this
   * page is about the server it is served by, and says so
   * (docs/decisions/d42-membership-lives-in-the-log.md). How far the others have
   * got, that server asks them as it answers
   * (docs/decisions/d47-status-asks-every-member.md).
   */
  import { api, ApiError, NetworkError } from "$lib/api";
  import type { ClusterMember, ClusterStatus } from "$lib/api";
  import { ago, exact } from "$lib/format";
  import { session } from "$lib/session.svelte";
  import Bar from "$lib/components/Bar.svelte";
  import Button from "$lib/components/Button.svelte";
  import Chip from "$lib/components/Chip.svelte";
  import Dialog from "$lib/components/Dialog.svelte";
  import Empty from "$lib/components/Empty.svelte";
  import Notice from "$lib/components/Notice.svelte";
  import Table from "$lib/components/Table.svelte";
  import type { Column } from "$lib/components/Table.svelte";

  type Member = ClusterStatus["members"][number];

  /**
   * single is a server with no cluster section, which the API answers with a
   * 404. It is a way to run, not a fault, and gets a page of its own.
   */
  let status = $state<ClusterStatus | null>(null);
  let single = $state(false);
  let loading = $state(true);
  let trouble = $state<string | null>(null);

  let starting = $state(false);
  let busy = $state(false);
  let notStarted = $state<string | null>(null);
  let started = $state<ClusterMember | null>(null);
  let copied = $state(false);

  /** removing is the member a removal is being asked about; itself, to leave. */
  let removing = $state<Member | null>(null);
  let refused = $state<string | null>(null);

  const leaving = $derived(removing !== null && removing.id === status?.self.id);
  /** out is a server that no longer takes part, either way it got there. */
  const out = $derived(Boolean(status?.behind || status?.removed));

  const administers = $derived(session.can("admin"));
  const joinLine = $derived(started ? `weg serve --join ${started.address}` : "");

  const roles: Record<Member["role"], { label: string; what: string }> = {
    voter: {
      label: "Voter",
      what: "Counts towards quorum, and can lead.",
    },
    nonvoter: {
      label: "Non-voter",
      what: "Receives the whole log and answers queries like any member, and is neither counted nor waited for.",
    },
    witness: {
      label: "Witness",
      what: "Votes and keeps the log, answers no queries, and hands leadership on when it wins an election.",
    },
  };

  const columns: Column[] = [
    { label: "Member" },
    { label: "Address", width: "14rem" },
    { label: "Role", width: "8rem" },
    { label: "Log", width: "9rem" },
    { label: "", width: "9rem" },
  ];

  /** furthest is the last commit any member reported, the leader's when it answered. */
  const furthest = $derived(
    Math.max(
      status?.committed ?? 0,
      ...(status?.members ?? []).map((m) => m.progress?.committed ?? 0),
    ),
  );

  /** log says how far one member has got, against the furthest commit. */
  function log(m: Member): { label: string; tone: "ok" | "warn" | "crit"; title?: string } {
    const p = m.progress;
    if (!p) return { label: "Not reached", tone: "warn", title: m.trouble };
    if (p.behind) {
      return {
        label: "Stopped",
        tone: "crit",
        title: `At ${where(p.behind.entry)}, because ${p.behind.reason}`,
      };
    }
    if (p.removed) return { label: "Left", tone: "crit" };
    if (p.applied < furthest) return { label: `${furthest - p.applied} behind`, tone: "warn" };
    return { label: "Current", tone: "ok" };
  }

  async function load() {
    loading = true;
    trouble = null;
    try {
      status = await api.get("/cluster");
      single = false;
    } catch (err) {
      status = null;
      single = err instanceof ApiError && err.status === 404;
      if (!single) {
        trouble =
          err instanceof NetworkError
            ? "The server did not answer."
            : err instanceof ApiError
              ? (err.detail ?? err.title)
              : "The cluster could not be read.";
      }
    } finally {
      loading = false;
    }
  }

  async function start() {
    busy = true;
    notStarted = null;
    try {
      started = await api.post("/cluster/init");
      await load();
    } catch (err) {
      notStarted =
        err instanceof ApiError ? (err.detail ?? err.title) : "The cluster was not started.";
    } finally {
      busy = false;
    }
  }

  async function copyJoin() {
    try {
      await navigator.clipboard.writeText(joinLine);
      copied = true;
    } catch {
      notStarted = "This browser did not allow the copy. Select the line instead.";
    }
  }

  function closeStart() {
    starting = false;
    started = null;
    notStarted = null;
    copied = false;
  }

  async function remove() {
    if (!removing) return;
    busy = true;
    refused = null;
    try {
      await api.delete("/cluster/members/{memberId}", { path: { memberId: removing.id } });
      removing = null;
      await load();
    } catch (err) {
      refused =
        err instanceof ApiError ? (err.detail ?? err.title) : "The member was not taken out.";
    } finally {
      busy = false;
    }
  }

  /** where names the place in the log a member stopped at. */
  const where = (entry: number) => (entry === 0 ? "a log snapshot" : `entry ${entry}`);

  $effect(() => {
    load();
  });
</script>

<svelte:head><title>Cluster — Wegweiser</title></svelte:head>

<Bar title="Cluster">
  {#snippet actions()}
    <Button onclick={load}>Refresh</Button>
  {/snippet}
</Bar>

<div class="flex flex-1 flex-col gap-5 overflow-auto py-5">
  {#if trouble}
    <div class="max-w-3xl px-5">
      <Notice tone="crit" title="The cluster could not be read">
        {trouble}
        {#snippet actions()}
          <Button onclick={load}>Try again</Button>
        {/snippet}
      </Notice>
    </div>
  {/if}

  {#if single}
    <Empty title="A single server">
      This server's configuration file has no cluster section, so it keeps its zones to
      itself. A cluster is a few servers holding the same zones, kept in step through one
      log, and each of them answers every change. Add the section below, restart the
      server, and start the cluster here.
      {#snippet actions()}
        <pre
          class="num mt-1 rounded-sm border border-line bg-sunken px-4 py-3 text-left
                 text-[12px] leading-relaxed text-ink">cluster:
  advertise: "192.0.2.1:8054"   # where the other members reach this one
  secret: "…"                   # openssl rand -base64 32, the same on every member</pre>
      {/snippet}
    </Empty>
  {:else if status && status.behind}
    <div class="max-w-3xl px-5">
      <Notice tone="crit" title="This server has left its cluster">
        It stopped at {where(status.behind.entry)},
        <span title={exact(status.behind.since)}>{ago(status.behind.since)}</span>, because
        {status.behind.reason}. It answers queries with what it held then and refuses
        writes. To repair it, remove it from the cluster on another member's Cluster page,
        discard its database and its Raft directory, and start it again with
        <code class="num">weg serve --join</code>.
      </Notice>
    </div>
  {:else if status && status.removed}
    <div class="max-w-3xl px-5">
      <Notice tone="crit" title="This server has left its cluster">
        It was taken out of the cluster, and answers queries with what it held then and refuses
        writes. To join it again, discard its database and its Raft directory and start it with
        <code class="num">weg serve --join</code>. To run it on its own, discard its Raft
        directory only.
      </Notice>
    </div>
  {:else if status && !status.replicating}
    <Empty title="In no cluster yet">
      This server has a cluster section, so it can be a member, and it is not one yet. Start
      a cluster with it here, and it brings everything it holds. Or start it with
      <code class="num">weg serve --join</code> and the address of a member, and it takes the
      cluster's zones instead.
      {#snippet actions()}
        {#if administers}
          <Button weight="primary" onclick={() => (starting = true)}>Start a cluster</Button>
        {:else}
          <span class="text-[12px] text-ink-faint">Starting one needs the admin scope.</span>
        {/if}
      {/snippet}
    </Empty>
  {/if}

  {#if status && status.replicating}
    <!--
      The plate: which server this is, in the voice the rail gives the product's
      own name, because every number on this page is this server's view and the
      reader has to know whose.
    -->
    <section
      class="relative mx-5 flex flex-wrap items-end gap-x-8 gap-y-2 border-l-[3px]
             border-signal py-1 pl-4"
      aria-label="This server"
    >
      <div class="min-w-0">
        <p class="text-[12px] text-ink-faint">This server</p>
        <p class="font-cond text-[30px] leading-none font-bold tracking-[0.08em] uppercase">
          {status.self.id}
        </p>
      </div>
      <dl class="flex flex-wrap gap-x-8 gap-y-1 pb-0.5 text-[12px]">
        <div>
          <dt class="text-ink-faint">Reached at</dt>
          <dd class="num text-ink">{status.self.address}</dd>
        </div>
        <div>
          <dt class="text-ink-faint">Log</dt>
          <dd class="num text-ink">
            {status.applied} of {status.committed} applied
          </dd>
        </div>
        <div class="self-end">
          {#if out}
            <Chip tone="crit">Left the cluster</Chip>
          {:else if status.applied < status.committed}
            <Chip tone="warn" title="Entries committed and not yet carried out here">
              {status.committed - status.applied} behind
            </Chip>
          {:else}
            <Chip tone="ok" dot>Current</Chip>
          {/if}
        </div>
      </dl>
    </section>

    <Table {columns} items={status.members} key={(m) => m.id}>
      {#snippet row(m: Member)}
        {@const shown = log(m)}
        <td class="py-1.5 pr-3 pl-5 whitespace-nowrap">
          {m.id}
          {#if m.id === status?.self.id}
            <span class="ml-2 text-[11px] text-ink-faint">this server</span>
          {/if}
        </td>
        <td class="num px-3 py-1.5 text-ink-mute">{m.address}</td>
        <td class="px-3 py-1.5">
          <Chip tone="neutral" title={roles[m.role].what}>{roles[m.role].label}</Chip>
        </td>
        <td class="px-3 py-1.5">
          <Chip tone={shown.tone} dot={shown.tone === "ok"} title={shown.title}>{shown.label}</Chip>
        </td>
        <td class="py-1.5 pr-5 pl-3">
          <div class="flex items-center gap-2">
            {#if m.leader}
              <Chip tone="signal" dot title="Takes every write, whichever member it was sent to">
                Leader
              </Chip>
            {/if}
            {#if administers && !out}
              <button
                type="button"
                onclick={() => ((removing = m), (refused = null))}
                aria-label={m.id === status?.self.id ? "Leave the cluster" : `Remove ${m.id}`}
                title={m.id === status?.self.id ? "Leave the cluster" : "Take it out of the cluster"}
                class="ml-auto grid size-6 cursor-pointer place-items-center rounded-xs text-ink-faint
                       opacity-0 transition-opacity group-hover:opacity-100 hover:bg-crit-lo
                       hover:text-crit focus-visible:opacity-100"
              >
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" class="size-3.5">
                  <circle cx="12" cy="12" r="9" />
                  <path d="m6 6 12 12" stroke-linecap="round" />
                </svg>
              </button>
            {/if}
          </div>
        </td>
      {/snippet}

      {#snippet empty()}
        <Empty title="No members listed">
          A server that has left its cluster no longer holds a copy of the configuration to
          read the members from.
        </Empty>
      {/snippet}
    </Table>

    <p class="max-w-3xl px-5 text-[12px] text-ink-faint">
      {#if status.removed}
        These are the members as this server last knew them. Open this page on one of them to
        see the cluster as it is now.
      {:else}
        Every member answers queries, and any of them takes a write and hands it to the leader.
        How far each has got is what it said when this page asked. One that did not answer
        within a couple of seconds is shown as not reached: from here, a server that is off
        and one that cannot be reached look the same.
      {/if}
    </p>
  {/if}
</div>

<Dialog bind:open={starting} title="Start a cluster" onclose={closeStart}>
  {#if started}
    <p class="text-[12px] text-ink-mute">
      The cluster is started, and {started.id} is its first member and leads it. Every other
      server joins from its first start, with a cluster section of its own, the same secret,
      and an empty database:
    </p>
    <div class="flex items-center gap-3">
      <pre
        class="num flex-1 overflow-auto rounded-sm border border-line bg-sunken px-4 py-2.5
               text-[12px] text-ink">{joinLine}</pre>
      <Button onclick={copyJoin}>{copied ? "Copied" : "Copy"}</Button>
    </div>
  {:else}
    <p class="text-[12px] text-ink-mute">
      {status?.self.id} becomes the first member of a new cluster, and everything it holds goes
      into it: zones, tokens, keys and settings. It is done once. Writes wait for the moment
      it takes, and go through the cluster's log from then on.
    </p>
  {/if}

  {#if notStarted}
    <Notice tone="crit" title="The cluster was not started">{notStarted}</Notice>
  {/if}

  {#snippet actions()}
    {#if started}
      <Button weight="primary" onclick={closeStart}>Done</Button>
    {:else}
      <Button weight="quiet" onclick={closeStart}>Cancel</Button>
      <Button weight="primary" disabled={busy} onclick={start}>
        {busy ? "Starting…" : "Start the cluster"}
      </Button>
    {/if}
  {/snippet}
</Dialog>

<Dialog
  open={removing !== null}
  onclose={() => (removing = null)}
  title={leaving ? "Leave the cluster" : `Remove ${removing?.id ?? ""}`}
>
  {#if leaving}
    <p class="text-[13px] text-ink-mute">
      {removing?.id} stops being a member. It goes on answering queries with what it holds, and
      refuses writes from then on. Joining it again takes an emptied database; running it on its
      own takes discarding its Raft directory.
    </p>
  {:else}
    <p class="text-[13px] text-ink-mute">
      {removing?.id} stops being a member. This is for a member that is off, or has stopped over
      a change it could not apply. If it is running, it goes on answering queries with what it
      held and refuses writes.
    </p>
    {#if removing?.role === "voter"}
      <Notice tone="warn" title="Take out a member that is off first">
        Removing a running voter while another voter is off can leave the rest without a
        majority, and then the cluster takes no writes at all.
      </Notice>
    {/if}
  {/if}

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
      disabled={busy}
      class="border-crit bg-crit text-ground hover:border-crit hover:bg-crit"
    >
      {busy ? "Taking it out…" : leaving ? "Leave" : "Remove it"}
    </Button>
  {/snippet}
</Dialog>
