<script lang="ts">
  /**
   * What is wrong with this zone as it stands.
   *
   * The rules the write path enforces, applied to what is already stored, plus
   * the diagnoses it accepts and probably should not have been asked to. The
   * two are told apart rather than listed together: a zone missing a glue
   * record is not in the same condition as one holding a record nothing can
   * answer.
   */
  import { api, ApiError, NetworkError } from "$lib/api";
  import type { Finding } from "$lib/api";
  import { session } from "$lib/session.svelte";
  import Button from "$lib/components/Button.svelte";
  import Empty from "$lib/components/Empty.svelte";
  import Notice from "$lib/components/Notice.svelte";

  let { data } = $props();

  const zone = $derived(data.zone);
  const writable = $derived(session.can("write"));

  let findings = $state<Finding[]>([]);
  let records = $state(0);
  let truncated = $state(false);
  let checked = $state(false);
  let running = $state(false);
  let trouble = $state<string | null>(null);

  /**
   * Reverse is asked for rather than assumed: working it out plans the write
   * that would fix it, which holds the zone while it runs.
   */
  let reverse = $state(false);
  let reconciling = $state(false);
  /** The record whose claim is being written, so one button at a time waits. */
  let claiming = $state<string | null>(null);
  let reconciled = $state<string | null>(null);

  const errors = $derived(findings.filter((f) => f.severity === "error").length);
  const warnings = $derived(findings.length - errors);
  const missingReverse = $derived(findings.some((f) => f.scope === "reverse"));

  /** check is the button: a fresh answer, and last time's outcome cleared. */
  async function check() {
    reconciled = null;
    await run();
  }

  async function run() {
    running = true;
    trouble = null;
    try {
      const report = await api.get("/zones/{zoneId}/check", {
        path: { zoneId: zone.id },
        query: { reverse },
      });
      findings = report.findings;
      records = report.records;
      truncated = report.truncated;
      checked = true;
    } catch (err) {
      trouble =
        err instanceof NetworkError
          ? "The server did not answer."
          : err instanceof ApiError
            ? (err.detail ?? err.title)
            : "The zone could not be checked.";
    } finally {
      running = false;
    }
  }

  /**
   * makeCanonical hands the reverse entry for an address to the record a
   * finding names. Several names on one address is ordinary and only one of
   * them can be the reverse answer; this is how somebody says which.
   */
  async function makeCanonical(record: string) {
    claiming = record;
    trouble = null;
    try {
      await api.post("/records/{recordId}/canonical", { path: { recordId: record } });
      await run();
      reconciled = "The address now reverses to that name.";
    } catch (err) {
      trouble =
        err instanceof ApiError ? (err.detail ?? err.title) : "The entry could not be handed over.";
    } finally {
      claiming = null;
    }
  }

  /** reconcile writes the entries the check just said were missing. */
  async function reconcile() {
    reconciling = true;
    trouble = null;
    try {
      const result = await api.post("/zones/{zoneId}/reconcile", {
        path: { zoneId: zone.id },
      });
      const outcome = result.commit
        ? `Written, and the zone is now at serial ${result.commit.serialTo}.`
        : "The zone needed nothing after all.";
      await run();
      reconciled = outcome;
    } catch (err) {
      trouble =
        err instanceof ApiError ? (err.detail ?? err.title) : "The entries could not be written.";
    } finally {
      reconciling = false;
    }
  }

  /** count writes a number with its noun, so nothing says "1 findings". */
  function count(n: number, word: string): string {
    return `${n} ${word}${n === 1 ? "" : "s"}`;
  }

  /** summary says what was found, keeping the two kinds apart. */
  const summary = $derived(
    warnings === 0
      ? count(errors, "error")
      : errors === 0
        ? count(warnings, "warning")
        : `${count(errors, "error")} and ${count(warnings, "warning")}`,
  );
</script>

<div class="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-5">
  <div class="flex flex-wrap items-center gap-3">
    <Button weight="primary" onclick={check} disabled={running}>
      {running ? "Checking…" : checked ? "Check again" : "Check this zone"}
    </Button>

    <label class="flex items-center gap-2 text-[13px] text-ink-mute">
      <input type="checkbox" bind:checked={reverse} class="size-3.5 accent-signal" />
      Include the reverse entries this zone is missing
    </label>

    {#if checked}
      <span class="text-[13px] text-ink-faint">
        {count(records, "record")} read
      </span>
    {/if}
  </div>

  {#if trouble}
    <Notice tone="crit" title="The check did not finish">{trouble}</Notice>
  {/if}

  {#if reconciled}
    <Notice tone="signal" title="The missing entries were written">{reconciled}</Notice>
  {/if}

  {#if checked && findings.length === 0}
    <div class="py-10">
      <Empty title="Nothing to report">
        Every rule this server enforces holds for {zone.name} as it is stored, and nothing
        about it looks like a mistake.
      </Empty>
    </div>
  {/if}

  {#if findings.length > 0}
    <p class="text-[13px] text-ink-mute">
      {summary} in {count(records, "record")}.
    </p>

    {#if missingReverse && writable}
      <Notice tone="signal" title="The missing reverse entries can be written">
        Reverse automation reacts to changes, so a zone that arrived after the records it
        should hold has nothing to react to. Filling it is one commit, and it only adds.
        {#snippet actions()}
          <Button weight="primary" onclick={reconcile} disabled={reconciling}>
            {reconciling ? "Writing…" : "Fill them in"}
          </Button>
        {/snippet}
      </Notice>
    {/if}

    <ul class="flex flex-col gap-2">
      {#each findings as finding, i (`${finding.scope}:${finding.name}:${i}`)}
        <li>
          <Notice
            tone={finding.severity === "error" ? "crit" : "warn"}
            title={finding.name}
          >
            <span class="mb-1 block font-cond text-[11px] tracking-[0.09em] uppercase opacity-70">
              {finding.severity} · {finding.scope}
            </span>
            {finding.detail}
            {#snippet actions()}
              {#if finding.record && writable}
                <Button
                  onclick={() => makeCanonical(finding.record!)}
                  disabled={claiming !== null}
                >
                  {claiming === finding.record ? "Handing over…" : "Make this the answer"}
                </Button>
              {/if}
            {/snippet}
          </Notice>
        </li>
      {/each}
    </ul>

    {#if truncated}
      <p class="text-[13px] text-ink-mute">
        The list stops here. A zone with this many findings has one fault rather than
        {findings.length}, and the rest would say the same thing again.
      </p>
    {/if}
  {/if}

  {#if !checked && !running && !trouble}
    <div class="py-10">
      <Empty title="Nothing has been checked yet">
        A zone edited only through this server stays sound, so this is usually quiet. What it
        reaches is data the write path never saw: written before a rule existed, or put there
        by a hand on the database file.
      </Empty>
    </div>
  {/if}
</div>
