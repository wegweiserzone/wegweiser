<script lang="ts">
  /**
   * What this server is and what it has been doing.
   */
  import { goto } from "$app/navigation";
  import { api, ApiError, NetworkError } from "$lib/api";
  import type { Commit, Health } from "$lib/api";
  import { ago, exact } from "$lib/format";
  import { read } from "$lib/metrics";
  import type { Readings } from "$lib/metrics";
  import { session } from "$lib/session.svelte";
  import { standing } from "$lib/standing.svelte";
  import ReadOnly from "$lib/components/ReadOnly.svelte";
  import Bar from "$lib/components/Bar.svelte";
  import Bars from "$lib/components/Bars.svelte";
  import Button from "$lib/components/Button.svelte";
  import Chip from "$lib/components/Chip.svelte";
  import Metric from "$lib/components/Metric.svelte";
  import Notice from "$lib/components/Notice.svelte";
  import Sparkline from "$lib/components/Sparkline.svelte";

  /** How often the counters are read again. The rate is the difference. */
  const every = 2000;

  /** How many rates the sparkline keeps: two minutes at that interval. */
  const kept = 60;

  let health = $state<Health | null>(null);
  let readings = $state<Readings | null>(null);
  let commits = $state<Commit[]>([]);
  let trouble = $state<string | null>(null);
  let refusedMetrics = $state<string | null>(null);

  let rates = $state<(number | null)[]>(new Array(kept).fill(null));
  let previous: { answered: number; at: number } | null = null;

  async function loadHealth() {
    try {
      health = await api.get("/healthz");
      trouble = null;
    } catch (err) {
      health = null;
      trouble =
        err instanceof NetworkError
          ? "The server did not answer."
          : "The server is up but not serving yet: no snapshot has been published, so there is nothing to answer queries from.";
    }
  }

  /** copies is where the secondaries stand: how many zone copies, and how many in step. */
  let copies = $state<{ all: number; inStep: number } | null>(null);

  async function loadCopies() {
    try {
      const pairs = await api.get("/secondary-status");
      copies = { all: pairs.length, inStep: pairs.filter((s) => s.state === "inStep").length };
    } catch {
      copies = null;
    }
  }

  async function loadCommits() {
    try {
      commits = (await api.get("/commits", { query: { limit: 6 } })).items;
    } catch {
      commits = [];
    }
  }

  async function loadMetrics() {
    try {
      const now = await read();
      const at = Date.now();

      // A rate is a difference. The first reading has nothing to differ from,
      // so it establishes the baseline and reports nothing.
      if (previous) {
        const seconds = (at - previous.at) / 1000;
        const rate = seconds > 0 ? Math.max(0, (now.answered - previous.answered) / seconds) : 0;
        rates = [...rates.slice(1), rate];
      }
      previous = { answered: now.answered, at };

      readings = now;
      refusedMetrics = null;
    } catch (err) {
      // The metrics need a credential like everything else, and a read-only
      // token has one. If this is refused it is worth saying once, quietly:
      // the rest of the screen still works.
      refusedMetrics =
        err instanceof ApiError
          ? (err.detail ?? err.title)
          : "The counters could not be read.";
    }
  }

  $effect(() => {
    loadHealth();
    loadCommits();
    loadCopies();
    loadMetrics();

    const ticking = setInterval(loadMetrics, every);
    return () => clearInterval(ticking);
  });

  const uptime = $derived.by(() => {
    if (!readings?.startedAt) return "—";
    const seconds = Math.max(0, (Date.now() - readings.startedAt.getTime()) / 1000);
    const days = Math.floor(seconds / 86400);
    const hours = Math.floor((seconds % 86400) / 3600);
    const minutes = Math.floor((seconds % 3600) / 60);
    if (days > 0) return `${days}d ${String(hours).padStart(2, "0")}h`;
    if (hours > 0) return `${hours}h ${String(minutes).padStart(2, "0")}m`;
    return `${minutes}m`;
  });

  const rate = $derived(rates.at(-1) ?? 0);

  type Fact = { tone: "ok" | "warn" | "crit"; text: string; href?: string; title?: string };

  /**
   * facts answer "is everything all right" before any number below does:
   * whether this server answers, whether its cluster takes writes, and whether
   * the secondaries have what it has. A fact with nothing to say is left out.
   */
  const facts = $derived.by(() => {
    const out: Fact[] = [];
    if (health) {
      const zones = `${health.zones} zone${health.zones === 1 ? "" : "s"}`;
      out.push(
        standing.out
          ? { tone: "crit", text: "Left its cluster, answering with what it held", href: "/cluster" }
          : health.zones === 0
            ? { tone: "warn", text: "Serving, and holding no zones yet" }
            : { tone: "ok", text: `Answering for ${zones}` },
      );
    } else if (trouble) {
      out.push({ tone: "crit", text: "Not answering queries" });
    }
    const v = standing.verdict;
    if (v) out.push({ tone: v.tone, text: `Cluster: ${v.headline.toLowerCase()}`, href: "/cluster", title: v.detail });
    if (copies && copies.all > 0) {
      const off = copies.all - copies.inStep;
      out.push(
        off === 0
          ? { tone: "ok", text: `Secondaries: all ${copies.all} copies in step`, href: "/secondaries" }
          : {
              tone: "warn",
              text: `Secondaries: ${off} of ${copies.all} copies not known to be in step`,
              href: "/secondaries",
            },
      );
    }
    return out;
  });

  const dot = { ok: "bg-ok", warn: "bg-warn", crit: "bg-crit" } as const;

  /**
   * The latency buckets worth drawing.
   */
  const latencyBars = $derived.by(() => {
    const buckets = readings?.latency ?? [];
    // Seven empty rows say less than one line saying nothing was answered.
    if (buckets.every((b) => b.count === 0)) return [];

    const finite = buckets.filter((b) => Number.isFinite(b.bound));
    const overflow = buckets.find((b) => !Number.isFinite(b.bound));

    let last = finite.findLastIndex((b) => b.count > 0);
    const target = finite.findIndex((b) => b.bound >= 0.001);
    last = Math.max(last, target, 2);

    const shown = finite.slice(0, last + 1).map((b) => ({
      label: b.bound < 0.001 ? `${(b.bound * 1e6).toFixed(0)} µs` : `${b.bound * 1000} ms`,
      count: b.count,
    }));

    // Anything past the last bucket has nowhere else to be said, and it is
    // exactly the thing somebody would want to know about.
    if (overflow && overflow.count > 0) {
      shown.push({ label: "slower", count: overflow.count });
    }
    return shown;
  });

  const rcodeTone = (label: string) =>
    label === "NOERROR"
      ? ("ok" as const)
      : label === "NXDOMAIN"
        ? ("warn" as const)
        : label === "SERVFAIL"
          ? ("crit" as const)
          : ("neutral" as const);

  const kindLabel: Record<string, string> = {
    edit: "Edit",
    zone_create: "Created",
    zone_update: "Settings",
    zone_delete: "Deleted",
    import: "Import",
    rollback: "Rollback",
  };
</script>

<svelte:head><title>Overview — Wegweiser</title></svelte:head>

<Bar title="Overview">
  {#snippet actions()}
    <Button
      onclick={() => {
        loadHealth();
        loadCommits();
        loadCopies();
        loadMetrics();
        standing.refresh();
      }}
    >
      Refresh
    </Button>
  {/snippet}
</Bar>

<!--
  The answer first: one line per thing that can be wrong, each leading to the
  page that explains it. Everything below is the detail.
-->
{#if facts.length > 0}
  <ul
    class="flex shrink-0 flex-wrap items-center gap-x-8 gap-y-2 border-b border-line px-5 py-3
           text-[13px]"
    aria-label="How this server stands"
  >
    {#each facts as fact (fact.text)}
      <li class="flex items-center gap-2">
        <span class="size-2 shrink-0 rounded-full {dot[fact.tone]}" aria-hidden="true"></span>
        {#if fact.href}
          <a
            href={fact.href}
            title={fact.title}
            class="{fact.tone === 'ok' ? 'text-ink' : fact.tone === 'warn' ? 'text-warn' : 'text-crit'}
                   underline-offset-2 hover:underline"
          >
            {fact.text}
          </a>
        {:else}
          <span class={fact.tone === "ok" ? "text-ink" : fact.tone === "warn" ? "text-warn" : "text-crit"}>
            {fact.text}
          </span>
        {/if}
      </li>
    {/each}
  </ul>
{/if}

<dl class="flex shrink-0 items-stretch overflow-x-auto border-b border-line bg-surface">
  <Metric label="Uptime">{uptime}</Metric>
  <Metric label="Zones">{health?.zones ?? "—"}</Metric>
  <Metric label="Records">{health?.records?.toLocaleString("en") ?? "—"}</Metric>
  <Metric label="Answered">
    {readings ? readings.answered.toLocaleString("en") : "—"}
  </Metric>
  <Metric label="Right now" unit="/s" tone={rate > 0 ? "signal" : undefined}>
    {rate.toFixed(rate > 0 && rate < 10 ? 1 : 0)}
  </Metric>
</dl>

<div class="flex flex-1 flex-col gap-6 overflow-auto px-5 py-5">
  {#if health && health.zones === 0}
    <!-- A server holding nothing has one next step, and it is not reading charts. -->
    <Notice tone="signal" title="No zones yet">
      A zone is a name this server answers for. Create one, or bring one over from another
      server as a zonefile, and it is answered as soon as it is written.
      {#snippet actions()}
        {#if standing.writes.allowed}
          <Button onclick={() => goto("/zones?import")}>Import a zonefile</Button>
          <Button weight="primary" onclick={() => goto("/zones?new")}>Create a zone</Button>
        {:else}
          <ReadOnly />
        {/if}
      {/snippet}
    </Notice>
  {/if}

  {#if trouble}
    <Notice tone="warn" title="Not answering queries">
      {trouble}
      {#snippet actions()}
        <Button onclick={loadHealth}>Check again</Button>
      {/snippet}
    </Notice>
  {/if}

  {#if refusedMetrics}
    <Notice tone="warn" title="The counters could not be read">
      {refusedMetrics} Everything else on this screen comes from elsewhere and still works.
    </Notice>
  {/if}

  <!-- Queries -->
  <section class="flex flex-col gap-2.5">
    <div class="flex items-baseline gap-3">
      <h2 class="sign text-[11px] text-ink-faint">Queries per second</h2>
      <p class="num hidden text-[11px] text-ink-faint sm:block">
        the last two minutes, from the difference between counter readings
      </p>
      <span class="num ml-auto text-[12px] text-ink-mute">
        peak {Math.max(0, ...rates.map((r) => r ?? 0)).toFixed(0)} /s
      </span>
    </div>
    <Sparkline series={rates} label="Queries per second over the last two minutes" />
  </section>

  <div class="grid gap-6 xl:grid-cols-2">
    <!-- What it answers -->
    <section class="flex flex-col gap-3">
      <div class="flex items-baseline gap-3">
        <h2 class="sign text-[11px] text-ink-faint">Answers</h2>
        <p class="text-[12px] text-ink-mute">Since this process started.</p>
      </div>
      <Bars
        readings={readings?.byRcode ?? []}
        total={readings?.answered ?? 0}
        tone={rcodeTone}
        empty="Nothing has been asked yet."
      />

      {#if readings && (readings.dropped > 0 || readings.truncated > 0)}
        <p class="num flex flex-wrap gap-4 border-t border-line-soft pt-2.5 text-[11px] text-ink-faint">
          {#if readings.truncated > 0}
            <span>
              {readings.truncated.toLocaleString("en")} truncated
              <span class="text-ink-mute">— cut to fit UDP and marked TC</span>
            </span>
          {/if}
          {#if readings.dropped > 0}
            <span>
              {readings.dropped.toLocaleString("en")} dropped
              <span class="text-ink-mute">— no safe reply existed</span>
            </span>
          {/if}
        </p>
      {/if}
    </section>

    <!-- What it is asked -->
    <section class="flex flex-col gap-3">
      <div class="flex items-baseline gap-3">
        <h2 class="sign text-[11px] text-ink-faint">Questions</h2>
        <p class="text-[12px] text-ink-mute">
          By type. Anything without a mnemonic is folded together.
        </p>
      </div>
      <Bars
        readings={(readings?.byType ?? []).slice(0, 7)}
        total={readings?.answered ?? 0}
        empty="Nothing has been asked yet."
      />
    </section>

    <!-- How fast -->
    <section class="flex flex-col gap-3">
      <div class="flex items-baseline gap-3">
        <h2 class="sign text-[11px] text-ink-faint">Latency</h2>
        <span class="num ml-auto text-[12px] text-ink-mute">
          {readings?.withinTarget != null
            ? `${(readings.withinTarget * 100).toFixed(2)}% under 1 ms`
            : "—"}
        </span>
      </div>
      <Bars
        readings={latencyBars}
        total={readings?.answered ?? 0}
        empty="Nothing has been answered yet."
      />
      <p class="text-[12px] text-ink-mute">
        Time from reading a query to having written its response. The target is a 99th
        percentile inside a millisecond.
      </p>
    </section>

    <!-- What changed -->
    <section class="flex flex-col gap-3">
      <div class="flex items-baseline gap-3">
        <h2 class="sign text-[11px] text-ink-faint">Recent changes</h2>
        <a
          href="/history"
          class="sign ml-auto text-[10px] text-ink-faint underline-offset-2 hover:text-ink hover:underline"
        >
          all of it
        </a>
      </div>

      {#if commits.length === 0}
        <p class="text-[13px] text-ink-faint">Nothing has been written yet.</p>
      {:else}
        <ul class="flex flex-col divide-y divide-line-soft border-y border-line-soft">
          {#each commits as commit (commit.id)}
            <li class="grid grid-cols-[5rem_minmax(0,1fr)_auto] items-center gap-3 py-1.5">
              <span class="num text-[11px] text-ink-faint">
                {commit.serialFrom}<span class="px-0.5">→</span><span class="text-ink"
                  >{commit.serialTo}</span
                >
              </span>
              <span class="flex min-w-0 items-center gap-2">
                <Chip>{kindLabel[commit.kind] ?? commit.kind}</Chip>
                <span class="num truncate text-[12px]">{commit.zoneName}</span>
              </span>
              <span
                class="num text-[11px] whitespace-nowrap text-ink-faint"
                title={exact(commit.createdAt)}
              >
                {ago(commit.createdAt)}
              </span>
            </li>
          {/each}
        </ul>
      {/if}
    </section>
  </div>

  <!-- The session, and where the rest of this lives -->
  <section class="flex flex-col gap-3 border-t border-line pt-5">
    <h2 class="sign text-[11px] text-ink-faint">This session</h2>
    <dl class="grid max-w-3xl grid-cols-[8rem_minmax(0,1fr)] gap-x-6 gap-y-1.5 text-[13px]">
      <dt class="text-ink-mute">Token</dt>
      <dd class="num">{session.who?.name ?? "—"}</dd>

      <dt class="text-ink-mute">Allowed</dt>
      <dd class="flex flex-wrap gap-1.5">
        {#each session.who?.scopes ?? [] as scope (scope)}
          <Chip tone={scope === "admin" ? "signal" : "neutral"}>{scope}</Chip>
        {/each}
      </dd>

      <dt class="text-ink-mute">Ends</dt>
      <dd class="num">
        {session.who?.expiresAt
          ? exact(session.who.expiresAt)
          : "when the server restarts"}
      </dd>

      <dt class="text-ink-mute">Snapshot</dt>
      <dd class="num" title={exact(readings?.snapshotAt?.toISOString())}>
        {readings?.snapshotAt ? ago(readings.snapshotAt.toISOString()) : "—"}
      </dd>

      <dt class="text-ink-mute">Version</dt>
      <dd class="num">{health?.version ?? "—"}</dd>
    </dl>
  </section>
</div>
