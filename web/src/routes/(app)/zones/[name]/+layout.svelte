<script lang="ts">
  /**
   * Everything about one zone: what it is at a glance, and the two things you
   * can be doing to it.
   */
  import { page } from "$app/state";
  import { ago, exact } from "$lib/format";
  import Bar from "$lib/components/Bar.svelte";
  import Metric from "$lib/components/Metric.svelte";

  let { data, children } = $props();

  const zone = $derived(data.zone);
  const base = $derived(`/zones/${encodeURIComponent(zone.name)}`);

  const tabs = $derived([
    { href: base, label: "Records" },
    { href: `${base}/settings`, label: "Settings" },
    // The history lives in one place for the whole server; this arrives there
    // already narrowed to this zone.
    { href: `/history?zone=${encodeURIComponent(zone.name)}`, label: "History" },
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
  {/snippet}
</Bar>

<dl class="flex shrink-0 items-stretch overflow-x-auto border-b border-line bg-surface">
  <Metric label="Kind">{zone.kind}</Metric>
  {#if zone.prefix}
    <Metric label="Network">{zone.prefix}</Metric>
  {/if}
  <Metric label="Serial">{zone.soa.serial}</Metric>
  <Metric label="Default TTL" unit="s">{zone.defaultTtl}</Metric>
  <Metric label="State" tone={zone.disabled ? "warn" : "ok"}>
    {zone.disabled ? "disabled" : "serving"}
  </Metric>
  <Metric label="Changed">
    <span title={exact(zone.updatedAt)}>{ago(zone.updatedAt)}</span>
  </Metric>
</dl>

{@render children()}
