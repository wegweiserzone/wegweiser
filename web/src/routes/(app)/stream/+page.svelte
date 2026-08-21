<script lang="ts">
  /**
   * Queries, as they are answered.
   *
   * The filter is the server's: it is applied before anything is buffered, so
   * watching one zone stays complete however busy the rest of the server is
   * (D9). Changing it reopens the stream, which is the honest way to say that
   * this is not a search over history; there is no history here, only what is
   * happening.
   */
  import { ApiError } from "$lib/api";
  import type { QueryEvent, StreamStatus } from "$lib/api";
  import { everyType } from "$lib/records";
  import { watchQueries } from "$lib/stream";
  import Bar from "$lib/components/Bar.svelte";
  import Button from "$lib/components/Button.svelte";
  import Chip from "$lib/components/Chip.svelte";
  import Empty from "$lib/components/Empty.svelte";
  import Histogram from "$lib/components/Histogram.svelte";
  import Notice from "$lib/components/Notice.svelte";
  import Sparkline from "$lib/components/Sparkline.svelte";

  /** kept is how many exchanges stay on screen. Oldest out, newest in. */
  const kept = 400;

  /** The response codes worth a button. Everything else is reachable by typing. */
  const rcodes = ["NOERROR", "NXDOMAIN", "REFUSED", "SERVFAIL"];

  /** Bucket edges in microseconds, and what to call them. */
  const edges = [10, 25, 50, 100, 250, 1000];
  const bucketLabels = ["<10µ", "25µ", "50µ", "100µ", "250µ", "1m", ">1m"];

  let rows = $state<QueryEvent[]>([]);
  let status = $state<StreamStatus | null>(null);
  let connected = $state(false);
  let trouble = $state<string | null>(null);
  let paused = $state(false);
  let missed = $state(0);

  // Filters, as the server takes them.
  let name = $state("");
  let client = $state("");
  let type = $state("");
  let rcode = $state("");

  // The last sixty seconds, one bucket a second.
  let perSecond = $state<number[]>(new Array(60).fill(0));
  let thisSecond = 0;
  let currentSecond = Math.floor(Date.now() / 1000);
  let buckets = $state<number[]>(new Array(7).fill(0));
  let seen = $state(0);

  function bucketOf(us: number): number {
    for (let i = 0; i < edges.length; i++) {
      if (us < (edges[i] ?? 0)) return i;
    }
    return edges.length;
  }

  function record(event: QueryEvent) {
    seen += 1;
    const bucket = bucketOf(event.latencyUs);
    buckets[bucket] = (buckets[bucket] ?? 0) + 1;

    const second = Math.floor(new Date(event.at).getTime() / 1000);
    if (second !== currentSecond) {
      // Every second between the last one and this one was quiet, and the
      // chart has to show the quiet as well as the noise.
      const gap = Math.min(60, Math.max(1, second - currentSecond));
      const next = [...perSecond.slice(gap), ...new Array(gap - 1).fill(0), thisSecond];
      perSecond = next.slice(-60);
      currentSecond = second;
      thisSecond = 0;
    }
    thisSecond += 1;

    if (paused) {
      missed += 1;
      return;
    }
    rows = [event, ...rows].slice(0, kept);
  }

  /** open starts a stream with the filter as it now stands. */
  function open() {
    rows = [];
    status = null;
    trouble = null;
    connected = false;
    missed = 0;

    // The charts go with the rows. They describe what this watcher is seeing,
    // and a new filter is a different population: carrying a peak from the
    // unfiltered stream into a chart above a filtered one states something
    // that was never measured.
    perSecond = new Array(60).fill(0);
    buckets = new Array(7).fill(0);
    thisSecond = 0;
    currentSecond = Math.floor(Date.now() / 1000);
    seen = 0;

    return watchQueries(
      {
        name,
        client,
        types: type ? [type] : [],
        rcodes: rcode ? [rcode] : [],
      },
      {
        onOpen: () => ((connected = true), (trouble = null)),
        onQuery: record,
        onStatus: (s) => (status = s),
        onError: (err) => {
          connected = false;
          trouble =
            err instanceof ApiError
              ? (err.detail ?? err.title)
              : "The stream ended. The server may be restarting.";
        },
      },
    );
  }

  // One stream at a time. Reading the filters is what reopens it when one of
  // them changes: the filter is the server's, so a new filter is a new
  // stream.
  $effect(() => {
    void name;
    void client;
    void type;
    void rcode;
    const stop = open();
    return stop;
  });

  const rate = $derived(perSecond.at(-1) ?? 0);
  const peak = $derived(Math.max(...perSecond));
  /**
   * The share answered inside a millisecond, which is what D12 sets as the
   * target. "Under 10 µs" was the first headline here and it is a worse one:
   * it reads as a failure on a server doing exactly what it should, because
   * a real socket round trip measured server-side lands in the tens of
   * microseconds and the ten-microsecond figure belongs to a benchmark.
   */
  const withinTarget = $derived.by(() => {
    if (seen === 0) return 100;
    const slow = buckets[6] ?? 0;
    return ((seen - slow) / seen) * 100;
  });
  const sampling = $derived((status?.ratio ?? 1) > 1);

  const tone: Record<string, "ok" | "warn" | "crit" | "neutral"> = {
    NOERROR: "ok",
    NXDOMAIN: "warn",
    SERVFAIL: "crit",
    REFUSED: "neutral",
  };

  function clock(at: string): string {
    const t = new Date(at);
    return (
      t.toTimeString().slice(0, 8) + "." + String(t.getMilliseconds()).padStart(3, "0")
    );
  }

  function latency(us: number): string {
    return us < 1000 ? `${us.toFixed(1)} µs` : `${(us / 1000).toFixed(2)} ms`;
  }

  /** The bar's length is logarithmic: this server's range spans four decades. */
  function barWidth(us: number): number {
    return Math.max(2, Math.min(46, Math.log10(Math.max(us, 1.2)) * 22));
  }
</script>

<svelte:head><title>Query stream — Wegweiser</title></svelte:head>

<Bar title="Query stream">
  {#snippet actions()}
    <span
      class="sign flex items-center gap-2 text-[12px] {connected && !paused
        ? 'text-signal'
        : 'text-ink-faint'}"
    >
      <span
        class="size-1.5 rounded-full bg-current {connected && !paused ? 'animate-pulse' : ''}"
      ></span>
      {#if !connected}
        Offline
      {:else if paused}
        Paused
      {:else}
        Live
      {/if}
    </span>

    <Button onclick={() => ((paused = !paused), (missed = 0))} disabled={!connected}>
      {paused ? "Resume" : "Pause"}
    </Button>
    <Button weight="quiet" onclick={() => (rows = [])}>Clear</Button>
  {/snippet}
</Bar>

<div class="grid shrink-0 border-b border-line bg-surface lg:grid-cols-[minmax(0,1fr)_22rem]">
  <div class="flex flex-col gap-2 px-5 py-3">
    <div class="flex items-baseline justify-between gap-3">
      <span class="sign text-[10px] text-ink-faint">Queries per second</span>
      <span class="num text-[12px] text-ink-mute">
        {rate.toLocaleString("en")} /s · peak {peak.toLocaleString("en")}
      </span>
    </div>
    <Sparkline series={perSecond} label="Queries per second over the last minute" />
  </div>

  <div class="flex flex-col gap-2 border-line-soft px-5 py-3 lg:border-l">
    <div class="flex items-baseline justify-between gap-3">
      <span class="sign text-[10px] text-ink-faint">Latency</span>
      <span class="num text-[12px] text-ink-mute">
        {seen === 0
          ? "nothing yet"
          : `${seen.toLocaleString("en")} seen · ${withinTarget.toFixed(1)}% under 1 ms`}
      </span>
    </div>
    <Histogram {buckets} labels={bucketLabels} />
  </div>
</div>

<div class="flex shrink-0 flex-wrap items-center gap-2 border-b border-line px-5 py-2.5">
  <label
    class="flex h-8 min-w-[15rem] items-center gap-2 rounded-sm border border-line bg-surface
           px-2.5 transition-colors focus-within:border-signal"
  >
    <span class="sign text-[10px] text-ink-faint">name</span>
    <input
      bind:value={name}
      placeholder="example.com"
      aria-label="Watch a name and everything below it"
      autocomplete="off"
      spellcheck="false"
      class="num min-w-0 flex-1 bg-transparent text-[12px] outline-none placeholder:text-ink-faint"
    />
  </label>

  <label
    class="flex h-8 min-w-[12rem] items-center gap-2 rounded-sm border border-line bg-surface
           px-2.5 transition-colors focus-within:border-signal"
  >
    <span class="sign text-[10px] text-ink-faint">from</span>
    <input
      bind:value={client}
      placeholder="192.168.0.0/24"
      aria-label="Watch one address or network"
      autocomplete="off"
      spellcheck="false"
      class="num min-w-0 flex-1 bg-transparent text-[12px] outline-none placeholder:text-ink-faint"
    />
  </label>

  <label class="flex items-center gap-2">
    <span class="sr-only">Filter by question type</span>
    <select
      bind:value={type}
      class="num h-8 cursor-pointer rounded-sm border border-line bg-surface px-2 text-[12px]
             text-ink outline-none focus:border-signal"
    >
      <option value="">any type</option>
      {#each everyType as option (option)}
        <option value={option}>{option}</option>
      {/each}
    </select>
  </label>

  <div class="flex items-center gap-1">
    {#each rcodes as option (option)}
      <button
        type="button"
        onclick={() => (rcode = rcode === option ? "" : option)}
        aria-pressed={rcode === option}
        class="sign h-8 cursor-pointer rounded-sm border border-line bg-surface px-2.5 text-[11px]
               text-ink-mute transition-colors hover:text-ink
               aria-pressed:border-signal aria-pressed:bg-signal-lo aria-pressed:text-signal"
      >
        {option}
      </button>
    {/each}
  </div>

  {#if paused && missed > 0}
    <span class="num ml-auto text-[11px] text-ink-faint">
      {missed.toLocaleString("en")} arrived while paused
    </span>
  {/if}
</div>

<div class="flex flex-1 flex-col overflow-auto">
  {#if trouble}
    <div class="px-5 pt-4">
      <Notice tone="crit" title="The stream is not running">
        {trouble}
        {#snippet actions()}
          <Button onclick={() => ((name = name), open())}>Try again</Button>
        {/snippet}
      </Notice>
    </div>
  {/if}

  {#if sampling && status}
    <div class="px-5 pt-4">
      <Notice tone="signal" title="Sampling">
        The filtered stream is faster than this watcher's cap, so one exchange in
        <b class="num">{status.ratio}</b> is being shown —
        <span class="num">{status.sent.toLocaleString("en")}</span> of
        <span class="num">{status.matched.toLocaleString("en")}</span> matched. The counts and
        the charts above are unaffected; narrow the filter to see every one.
      </Notice>
    </div>
  {/if}

  <table class="w-full border-collapse text-[13px]">
    <thead>
      <tr>
        {#each ["Time", "From", "Tr", "Name", "Type", "Rcode", "Size", "Latency"] as head, i (head)}
          <th
            scope="col"
            class="sign sticky top-0 z-10 border-b border-line bg-ground px-3 py-2 text-[11px]
                   whitespace-nowrap text-ink-faint select-none first:pl-5 last:pr-5
                   {i >= 6 ? 'text-right' : 'text-left'}"
          >
            {head}
          </th>
        {/each}
      </tr>
    </thead>

    <tbody>
      {#each rows as row (row.at + row.client + row.name + row.type + row.latencyUs)}
        <tr class="border-b border-line-soft">
          <td class="num py-1.5 pr-3 pl-5 text-[12px] text-ink-faint">{clock(row.at)}</td>
          <td class="num px-3 py-1.5 text-ink-mute">{row.client}</td>
          <td class="px-3 py-1.5"><Chip>{row.transport}</Chip></td>
          <td class="num max-w-[34ch] truncate px-3 py-1.5" title={row.name}>{row.name}</td>
          <td class="num px-3 py-1.5 text-[12px]">{row.type}</td>
          <td class="flex items-center gap-1.5 px-3 py-1.5">
            {#if row.dropped}
              <Chip tone="crit">dropped</Chip>
            {:else}
              <Chip tone={tone[row.rcode] ?? "neutral"}>{row.rcode}</Chip>
            {/if}
            {#if row.truncated}
              <Chip tone="info" title="Cut to fit the transport, RFC 1035 §4.1.1">TC</Chip>
            {/if}
          </td>
          <td class="num px-3 py-1.5 text-right text-ink-mute">{row.size}</td>
          <td class="num px-3 py-1.5 text-right">
            <span class="flex items-center justify-end gap-2">
              <span
                class="h-[3px] shrink-0 rounded-full {row.latencyUs > 250
                  ? 'bg-crit'
                  : row.latencyUs > 50
                    ? 'bg-warn'
                    : 'bg-ink-faint'}"
                style="width: {barWidth(row.latencyUs)}px"
              ></span>
              {latency(row.latencyUs)}
            </span>
          </td>
        </tr>
      {:else}
        <tr>
          <td colspan="8" class="px-5 py-16">
            {#if !connected}
              <Empty title="Not watching">
                The stream is not running. Whatever ended it is said above.
              </Empty>
            {:else}
              <Empty title="Nothing is being asked">
                This server is answering no queries that match. Send it one —
                <code class="num text-ink">dig @localhost example.com</code>, and it appears
                here as it is answered.
              </Empty>
            {/if}
          </td>
        </tr>
      {/each}
    </tbody>
  </table>
</div>
