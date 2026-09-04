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
  import type { SecondaryConfig, SecondaryFormat, SecondaryStanding } from "$lib/api";
  import { ago, exact } from "$lib/format";
  import { session } from "$lib/session.svelte";
  import Bar from "$lib/components/Bar.svelte";
  import Button from "$lib/components/Button.svelte";
  import Chip from "$lib/components/Chip.svelte";
  import Empty from "$lib/components/Empty.svelte";
  import Field from "$lib/components/Field.svelte";
  import Notice from "$lib/components/Notice.svelte";
  import Dialog from "$lib/components/Dialog.svelte";
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

  /**
   * The other half of the job this page is for: the file the second nameserver
   * needs. In a dialog rather than on the page, because what brings somebody
   * here is the table, and setting a secondary up is an errand with a beginning
   * and an end.
   */
  let setting = $state(false);
  let format = $state<SecondaryFormat>("bind");
  let primary = $state("");
  let farEnd = $state("");
  let written = $state<SecondaryConfig | null>(null);
  let writing = $state(false);
  let notWritten = $state<string | null>(null);
  let copied = $state(false);

  const administers = $derived(session.can("admin"));

  /** The software a configuration can be written for. */
  const formats: { value: SecondaryFormat; label: string }[] = [
    { value: "bind", label: "BIND" },
    { value: "knot", label: "Knot" },
  ];

  /** formatLabel names the software the way it writes its own name. */
  const formatLabel = (value: SecondaryFormat) =>
    formats.find((f) => f.value === value)?.label ?? value;

  /**
   * The address a secondary reaches this server at is asked for rather than
   * worked out. A server does not know which of its addresses the world uses,
   * and a hidden primary is named by no record to ask (docs/decisions/ D34).
   */
  async function writeConfig() {
    if (!administers || primary.trim() === "") return;
    writing = true;
    notWritten = null;
    copied = false;

    const query: { format: SecondaryFormat; primary: string; secondary?: string } = {
      format,
      primary: primary.trim(),
    };
    if (farEnd.trim() !== "") query.secondary = farEnd.trim();

    try {
      written = await api.get("/secondary-config", { query });
    } catch (err) {
      written = null;
      notWritten =
        err instanceof ApiError ? (err.detail ?? err.title) : "The configuration was not written.";
    } finally {
      writing = false;
    }
  }

  /**
   * The file is long enough that selecting it by hand is a chore. Where the
   * browser refuses the clipboard, say so rather than appearing to have
   * copied it.
   */
  async function copyConfig() {
    if (written === null) return;
    try {
      await navigator.clipboard.writeText(written.content);
      copied = true;
    } catch {
      notWritten = "This browser did not allow the copy. Select the text instead.";
    }
  }

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
    <Button onclick={() => (setting = true)}>Set one up</Button>
  {/snippet}
</Bar>

<div class="flex flex-1 flex-col gap-4 overflow-auto py-4">
  {#if trouble}
    <div class="max-w-3xl px-5">
      <Notice tone="crit" title="The secondaries could not be listed">
        {trouble}
        {#snippet actions()}
          <Button onclick={load}>Try again</Button>
        {/snippet}
      </Notice>
    </div>
  {/if}

  {#if !loading && !trouble && standing.length > 0 && wanting > 0}
    <div class="max-w-3xl px-5">
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

<Dialog bind:open={setting} title="Set a secondary up" size="wide">
  <p class="text-[12px] text-ink-mute">
    This server's half of the arrangement is the two lists under Settings. This is the other
    half, written out for the software running on the second nameserver: every zone here, the
    reverse ones among them, with the transfer key, its secret and its algorithm spelled the
    way that program spells it. It is a file to move. This server does not install it, and
    does not reach the machine it belongs on.
  </p>

  {#if !administers}
    <Notice tone="signal" title="Not allowed">
      The file carries a transfer key's secret, so writing it needs a token with the admin
      scope, the same as reading that secret does.
    </Notice>
  {:else}
    <div class="flex flex-wrap items-end gap-3">
      <div class="min-w-56 flex-1">
        <Field
          label="This server's address"
          bind:value={primary}
          disabled={writing}
          placeholder="192.0.2.1"
        />
      </div>
      <div class="min-w-56 flex-1">
        <Field
          label="The secondary's address"
          bind:value={farEnd}
          disabled={writing}
          placeholder="198.51.100.53"
        />
      </div>
    </div>
    <p class="text-[12px] text-ink-mute">
      The first is where a secondary reaches this server, and it has to be given: a server
      does not know which of its addresses the world uses, and a hidden primary is named by no
      record to ask. The second is optional and goes nowhere in the file. Naming it is what
      lets the two lists be checked against it rather than only described.
    </p>

    <div class="flex flex-wrap items-center gap-3">
      <div class="flex gap-px overflow-hidden rounded-sm border border-line">
        {#each formats as f (f.value)}
          <button
            type="button"
            onclick={() => (format = f.value)}
            aria-pressed={format === f.value}
            class="sign cursor-pointer bg-surface px-3 py-1.5 text-[11px] text-ink-mute
                   transition-colors hover:bg-raised aria-pressed:bg-signal-lo
                   aria-pressed:text-signal"
          >
            {f.label}
          </button>
        {/each}
      </div>
      <Button weight="primary" disabled={writing || primary.trim() === ""} onclick={writeConfig}>
        {writing ? "Writing…" : "Write it"}
      </Button>
    </div>

    {#if notWritten}
      <Notice tone="crit" title="The configuration was not written">{notWritten}</Notice>
    {/if}

    {#if written}
      {#if written.warnings.length > 0}
        <Notice tone="warn" title="The file is written; the arrangement is not finished">
          <ul class="flex list-disc flex-col gap-1 pl-4">
            {#each written.warnings as warning (warning)}
              <li>{warning}</li>
            {/each}
          </ul>
        </Notice>
      {/if}

      <div class="flex flex-col gap-2">
        <div class="flex items-center justify-between gap-3">
          <span class="sign text-[11px] text-ink-faint">For {formatLabel(written.format)}</span>
          <Button onclick={copyConfig}>{copied ? "Copied" : "Copy"}</Button>
        </div>
        <pre
          class="num max-h-72 overflow-auto rounded-sm border border-line bg-sunken p-4
                 text-[12px] leading-relaxed text-ink">{written.content}</pre>
      </div>
    {/if}
  {/if}

  {#snippet actions()}
    <Button weight="quiet" onclick={() => (setting = false)}>Close</Button>
  {/snippet}
</Dialog>
