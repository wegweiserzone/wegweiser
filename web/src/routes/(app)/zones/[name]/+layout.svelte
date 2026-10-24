<script lang="ts">
  /**
   * Everything about one zone: what it is at a glance, and the two things you
   * can be doing to it.
   */
  import { page } from "$app/state";
  import { api, ApiError } from "$lib/api";
  import type { Commit } from "$lib/api";
  import { ago, count, exact } from "$lib/format";
  import Bar from "$lib/components/Bar.svelte";
  import Button from "$lib/components/Button.svelte";
  import Chip from "$lib/components/Chip.svelte";
  import Notice from "$lib/components/Notice.svelte";

  let { data, children } = $props();

  const zone = $derived(data.zone);
  const base = $derived(`/zones/${encodeURIComponent(zone.name)}`);

  let exporting = $state(false);
  let exportFailed = $state<string | null>(null);

  /**
   * Export hands the zonefile over as a download rather than showing it: a zone
   * of any size is not something to read in a dialog, and a file is what the
   * next tool expects anyway. The name is the apex, so a directory of exports
   * sorts by zone.
   */
  async function exportZone() {
    exporting = true;
    exportFailed = null;
    try {
      const text = await api.exportZone(zone.id);
      const url = URL.createObjectURL(new Blob([text], { type: "text/dns" }));
      const a = document.createElement("a");
      a.href = url;
      a.download = `${zone.name.replace(/\.$/, "")}.zone`;
      a.click();
      URL.revokeObjectURL(url);
    } catch (err) {
      exportFailed =
        err instanceof ApiError ? (err.detail ?? err.title) : "The zone could not be exported.";
    } finally {
      exporting = false;
    }
  }

  const tabs = $derived([
    { href: base, label: "Records" },
    { href: `${base}/check`, label: "Check" },
    { href: `${base}/settings`, label: "Settings" },
    // The history lives in one place for the whole server; this arrives there
    // already narrowed to this zone.
    { href: `/history?zone=${encodeURIComponent(zone.name)}`, label: "History" },
  ]);

  /*
   * Two things read beside the zone, each optional: the last commit, and where
   * the copies on secondaries stand. One that cannot be read leaves its place
   * empty rather than the page broken. Read again whenever the serial moves.
   *
   * A check is not among them. It walks every record of the zone, and D31
   * shows a diagnosis unasked only when it costs no walk.
   */
  let last = $state<Commit | null>(null);
  let copies = $state<{ all: number; inStep: number } | null>(null);

  $effect(() => {
    const id = zone.id;
    const name = zone.name;
    void zone.soa.serial;
    void api
      .get("/commits", { query: { zoneId: id, limit: 1 } })
      .then((r) => (last = r.items[0] ?? null))
      .catch(() => (last = null));
    void api
      .get("/secondary-status")
      .then((all) => {
        const mine = all.filter((pair) => pair.zone === name);
        copies = mine.length
          ? { all: mine.length, inStep: mine.filter((p) => p.state === "inStep").length }
          : null;
      })
      .catch(() => (copies = null));
  });

  const facts = $derived<{ label: string; value: string; title?: string; href?: string }[]>([
    { label: "Kind", value: zone.kind },
    ...(zone.prefix ? [{ label: "Network", value: zone.prefix }] : []),
    ...(zone.records !== undefined ? [{ label: "Records", value: count(zone.records) }] : []),
    { label: "Serial", value: String(zone.soa.serial) },
    { label: "Default TTL", value: `${zone.defaultTtl} s` },
    ...(copies
      ? [{ label: "Secondaries", value: `${copies.inStep}/${copies.all} in step`, href: "/secondaries" }]
      : []),
  ]);
</script>

<svelte:head><title>{zone.name} — Wegweiser</title></svelte:head>

<Bar title="Zones" subject={zone.name}>
  {#snippet actions()}
    <nav class="flex items-center gap-0.5" aria-label="Zone">
      {#each tabs as tab (tab.href)}
        <a
          href={tab.href}
          aria-current={page.url.pathname === tab.href ? "page" : undefined}
          class="sign flex h-8 items-center rounded-sm px-3 text-[13px] text-ink-mute
                 transition-colors hover:bg-raised hover:text-ink
                 aria-[current=page]:bg-signal-lo aria-[current=page]:text-signal"
        >
          {tab.label}
        </a>
      {/each}
    </nav>
    <Button onclick={exportZone} disabled={exporting}>
      {exporting ? "Exporting…" : "Export"}
    </Button>
  {/snippet}
</Bar>

{#if exportFailed}
  <div class="px-5 pt-4">
    <Notice tone="crit" title="The zone could not be exported">{exportFailed}</Notice>
  </div>
{/if}

<!-- One line of facts rather than a row of tiles: they are read at a glance,
     and the records below are what the page is for. Serving is the usual
     state and goes unsaid; a zone that is not answered says so first. -->
<dl
  class="flex shrink-0 flex-wrap items-baseline gap-x-6 gap-y-1 border-b border-line bg-surface
         px-5 py-2.5"
>
  {#if zone.disabled}
    <div class="flex items-baseline gap-2">
      <dt class="sr-only">State</dt>
      <dd><Chip tone="warn" dot>Disabled, not answered</Chip></dd>
    </div>
  {/if}
  {#each facts as fact (fact.label)}
    <div class="flex items-baseline gap-2">
      <dt class="sign text-[10px] text-ink-faint">{fact.label}</dt>
      <dd class="num text-[13px]" title={fact.title}>
        {#if fact.href}
          <a href={fact.href} class="hover:text-signal hover:underline">{fact.value}</a>
        {:else}
          {fact.value}
        {/if}
      </dd>
    </div>
  {/each}
  {#if last}
    <!-- Who touched it last and what they did, which is the first thing asked
         about a zone that behaves differently from yesterday. -->
    <div class="flex min-w-0 items-baseline gap-2">
      <dt class="sign text-[10px] text-ink-faint">Last change</dt>
      <dd class="min-w-0 truncate text-[13px]" title={exact(last.createdAt)}>
        <a
          href="/history?zone={encodeURIComponent(zone.name)}"
          class="hover:text-signal hover:underline"
        >
          <span class="num">{ago(last.createdAt)}</span>
          {#if last.actor}
            <span class="text-ink-mute">by</span>
            {last.actor}{/if}{#if last.comment}<span class="text-ink-mute">:</span>
            <span class="num">{last.comment}</span>
          {/if}
        </a>
      </dd>
    </div>
  {/if}
</dl>

{@render children()}
