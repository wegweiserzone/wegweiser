<script lang="ts">
  /**
   * Where each secondary stands on each zone.
   *
   * This server writes the configuration a secondary needs and hands it over;
   * whether the copy was taken is a fact that lives on the other machine, and a
   * secondary that answers a notification and then fails to transfer looks
   * exactly like one that is working. So it is asked (docs/decisions/, D36).
   */
  import { goto } from "$app/navigation";
  import { api, ApiError, NetworkError } from "$lib/api";
  import type { SecondaryStanding } from "$lib/api";
  import { ago, exact } from "$lib/format";
  import Bar from "$lib/components/Bar.svelte";
  import Button from "$lib/components/Button.svelte";
  import Chip from "$lib/components/Chip.svelte";
  import Empty from "$lib/components/Empty.svelte";
  import Notice from "$lib/components/Notice.svelte";
  import Table from "$lib/components/Table.svelte";
  import type { Column } from "$lib/components/Table.svelte";

  type State = SecondaryStanding["state"];
  type Tone = "neutral" | "ok" | "warn" | "info";

  /**
   * What each state is called and how loudly. Only being in step is quiet.
   * Nothing here is critical: a zone that has not arrived yet, or a secondary
   * that has gone quiet, is something to look at rather than a failure of this
   * server, and colouring it red would teach the reader to ignore red.
   */
  const states: Record<State, { label: string; tone: Tone; what: string }> = {
    inStep: {
      label: "In step",
      tone: "ok",
      what: "It holds the serial this server publishes.",
    },
    behind: {
      label: "Behind",
      tone: "warn",
      what: "It holds an older serial. This is the fault the generated configuration cannot rule out.",
    },
    unasked: {
      label: "Unasked",
      tone: "neutral",
      what: "Nothing has come back for this pair yet. It is not the same as up to date.",
    },
    silent: {
      label: "Silent",
      tone: "warn",
      what: "It did not answer in time. The serial shown is the last one anybody saw.",
    },
    noSerial: {
      label: "No serial",
      tone: "warn",
      what: "It answered without a start of authority to read, which is what a refusal looks like from here.",
    },
    ahead: {
      label: "Ahead",
      tone: "info",
      what: "It holds a newer serial than this server publishes, so it took its copy from somewhere this server is not.",
    },
    unordered: {
      label: "Unordered",
      tone: "info",
      what: "The two serials are exactly half the space apart, which RFC 1982 §3.2 declines to order.",
    },
  };

  let standing = $state<SecondaryStanding[]>([]);
  let loading = $state(true);
  let trouble = $state<string | null>(null);

  const columns: Column[] = [
    { label: "Secondary", width: "14rem" },
    { label: "Zone" },
    { label: "State", width: "9rem" },
    { label: "Serial", align: "right", width: "8rem" },
    { label: "Behind", align: "right", width: "7rem" },
    { label: "Asked", align: "right", width: "8rem" },
  ];

  /** How many zones are not known to be in step, which is what to look at. */
  const wanting = $derived(standing.filter((s) => s.state !== "inStep").length);

  async function load() {
    loading = true;
    trouble = null;
    try {
      standing = await api.get("/secondary-status");
    } catch (err) {
      standing = [];
      trouble =
        err instanceof NetworkError
          ? "The server did not answer."
          : err instanceof ApiError
            ? (err.detail ?? err.title)
            : "The secondaries could not be read.";
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    load();
  });
</script>

<svelte:head><title>Secondaries — Wegweiser</title></svelte:head>

<Bar title="Secondaries">
  {#snippet actions()}
    <Button onclick={load}>Refresh</Button>
  {/snippet}
</Bar>

<div class="flex flex-1 flex-col overflow-auto">
  {#if trouble}
    <div class="px-5 pt-4">
      <Notice tone="crit" title="The secondaries could not be listed">
        {trouble}
        {#snippet actions()}
          <Button onclick={load}>Try again</Button>
        {/snippet}
      </Notice>
    </div>
  {/if}

  {#if !loading && !trouble && standing.length > 0 && wanting > 0}
    <div class="px-5 pt-4">
      <Notice tone="warn" title="{wanting} of {standing.length} not known to be in step">
        A secondary is asked once a notification to it has been answered or given up on, and
        at least hourly whether anything changed or not. Nothing is reported while a
        notification is still outstanding, so a zone here has had its chance.
      </Notice>
    </div>
  {/if}

  <Table {columns} items={standing} key={(s) => `${s.target} ${s.zone}`}>
    {#snippet row(s: SecondaryStanding)}
      <td class="num py-1.5 pr-3 pl-5 text-ink-mute">{s.target}</td>
      <td class="py-1.5 pr-3 pl-3">{s.zone}</td>
      <td class="px-3 py-1.5">
        <Chip tone={states[s.state].tone} dot={s.state === "inStep"} title={states[s.state].what}>
          {states[s.state].label}
        </Chip>
      </td>
      <td class="num px-3 py-1.5 text-right text-ink-mute">
        {s.serial ?? "—"}
      </td>
      <td class="num px-3 py-1.5 text-right {s.lag ? 'text-warn' : 'text-ink-faint'}">
        {#if s.lag === undefined}
          <span title="Not behind, or not known">—</span>
        {:else}
          <span title="{s.lag} commits this secondary has yet to see">{s.lag}</span>
        {/if}
      </td>
      <td
        class="num py-1.5 pr-5 pl-3 text-right text-[12px] text-ink-faint"
        title={s.askedAt ? exact(s.askedAt) : "Never asked"}
      >
        {s.askedAt ? ago(s.askedAt) : "never"}
      </td>
    {/snippet}

    {#snippet empty()}
      <Empty title={loading ? "Reading" : "Nobody to ask"}>
        {#if !loading}
          Who is asked is who is told a zone changed, and that list starts empty. Name a
          secondary under Settings and this fills in.
        {/if}
        {#snippet actions()}
          {#if !loading}
            <Button onclick={() => goto("/settings")}>Open settings</Button>
          {/if}
        {/snippet}
      </Empty>
    {/snippet}
  </Table>
</div>
