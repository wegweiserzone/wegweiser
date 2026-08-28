<script lang="ts">
  /**
   * Everything about one zone: what it is at a glance, and the two things you
   * can be doing to it.
   */
  import { page } from "$app/state";
  import { api, ApiError } from "$lib/api";
  import { ago, exact } from "$lib/format";
  import Bar from "$lib/components/Bar.svelte";
  import Button from "$lib/components/Button.svelte";
  import Metric from "$lib/components/Metric.svelte";
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
