<script lang="ts">
  /**
   * The keys a secondary signs a zone transfer with (RFC 8945).
   *
   * Unlike a token, a secret can be read back here. Verifying a signature means
   * recomputing it, so the server has to keep it, and hiding it in the
   * interface would be theatre rather than a boundary
   * (docs/decisions/d28-tsig.md). Reading one is still a deliberate act, so that a
   * secret appears when somebody asked for it.
   */
  import { api, ApiError, NetworkError } from "$lib/api";
  import type { TSIGAlgorithm, TSIGKey } from "$lib/api";
  import { ago, exact } from "$lib/format";
  import { session } from "$lib/session.svelte";
  import Bar from "$lib/components/Bar.svelte";
  import Button from "$lib/components/Button.svelte";
  import Chip from "$lib/components/Chip.svelte";
  import Dialog from "$lib/components/Dialog.svelte";
  import Empty from "$lib/components/Empty.svelte";
  import Field from "$lib/components/Field.svelte";
  import Notice from "$lib/components/Notice.svelte";
  import Table from "$lib/components/Table.svelte";
  import type { Column } from "$lib/components/Table.svelte";

  /**
   * RFC 8945 §6 also makes hmac-sha1 MUST-implement and permits HMAC-MD5. The
   * same table calls the first NOT RECOMMENDED and forbids the second, so
   * neither is offered; D28 argues the departure.
   */
  const algorithms: { value: TSIGAlgorithm; label: string; what: string }[] = [
    {
      value: "hmac-sha256.",
      label: "hmac-sha256",
      what: "What every current secondary speaks, and the one to pick unless the other end says otherwise.",
    },
    { value: "hmac-sha384.", label: "hmac-sha384", what: "Longer digest, longer secret." },
    { value: "hmac-sha512.", label: "hmac-sha512", what: "Longer again." },
  ];

  let keys = $state<TSIGKey[]>([]);
  let loading = $state(true);
  let trouble = $state<string | null>(null);

  let creating = $state(false);
  let newName = $state("");
  let newAlgorithm = $state<TSIGAlgorithm>("hmac-sha256.");
  let newSecret = $state("");
  let working = $state(false);
  let refused = $state<string | null>(null);

  /** The key whose secret is on screen, and the secret itself. */
  let shown = $state<{ key: TSIGKey; secret: string } | null>(null);
  let copied = $state(false);

  let revoking = $state<TSIGKey | null>(null);

  const columns: Column[] = [
    { label: "Name" },
    { label: "Algorithm", width: "11rem" },
    { label: "Created", align: "right", width: "9rem" },
    { label: "State", width: "9rem" },
    { label: "", width: "10rem" },
  ];

  const allowed = $derived(session.can("admin"));

  async function load() {
    loading = true;
    trouble = null;
    try {
      keys = await api.get("/tsig-keys");
    } catch (err) {
      keys = [];
      trouble =
        err instanceof NetworkError
          ? "The server did not answer."
          : err instanceof ApiError
            ? (err.detail ?? err.title)
            : "The keys could not be read.";
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    load();
  });

  async function create(event: SubmitEvent) {
    event.preventDefault();
    working = true;
    refused = null;
    try {
      const body: { name: string; algorithm: TSIGAlgorithm; secret?: string } = {
        name: newName.trim(),
        algorithm: newAlgorithm,
      };
      // An empty field means generate one, which is what an operator
      // configuring both ends wants.
      if (newSecret.trim()) body.secret = newSecret.trim();

      const result = await api.post("/tsig-keys", { body });
      creating = false;
      newName = "";
      newSecret = "";
      copied = false;
      shown = { key: result.key, secret: result.secret };
      await load();
    } catch (err) {
      refused =
        err instanceof ApiError ? (err.detail ?? err.title) : "The key could not be created.";
    } finally {
      working = false;
    }
  }

  async function reveal(key: TSIGKey) {
    refused = null;
    copied = false;
    try {
      const result = await api.get("/tsig-keys/{keyId}/secret", { path: { keyId: key.id } });
      shown = { key: result.key, secret: result.secret };
    } catch (err) {
      refused =
        err instanceof ApiError ? (err.detail ?? err.title) : "The secret could not be read.";
    }
  }

  async function revoke() {
    if (!revoking) return;
    working = true;
    refused = null;
    try {
      await api.delete("/tsig-keys/{keyId}", { path: { keyId: revoking.id } });
      revoking = null;
      shown = null;
      await load();
    } catch (err) {
      refused =
        err instanceof ApiError ? (err.detail ?? err.title) : "The key could not be withdrawn.";
    } finally {
      working = false;
    }
  }

  async function copy(secret: string) {
    try {
      await navigator.clipboard.writeText(secret);
      copied = true;
    } catch {
      // Clipboard access can be refused. The secret is on the screen either
      // way, which is what matters.
    }
  }

  const short = (algorithm: string) => algorithm.replace(/\.$/, "");
</script>

<svelte:head><title>Keys — Wegweiser</title></svelte:head>

<Bar title="Transfer keys">
  {#snippet actions()}
    {#if allowed}
      <Button weight="primary" onclick={() => ((creating = true), (refused = null))}>
        + New key
      </Button>
    {/if}
  {/snippet}
</Bar>

<div class="flex flex-1 flex-col overflow-auto">
  {#if !allowed}
    <div class="px-5 pt-4">
      <Notice tone="warn" title="This session may not manage keys">
        A key that may transfer may take every zone, so managing one needs the admin scope.
      </Notice>
    </div>
  {/if}

  {#if trouble}
    <div class="px-5 pt-4">
      <Notice tone="crit" title="The keys could not be listed">
        {trouble}
        {#snippet actions()}
          <Button onclick={load}>Try again</Button>
        {/snippet}
      </Notice>
    </div>
  {/if}

  {#if refused && !creating && !revoking}
    <div class="px-5 pt-4">
      <Notice tone="crit" title="That did not work">{refused}</Notice>
    </div>
  {/if}

  {#if shown}
    <div class="px-5 pt-4">
      <Notice tone="signal" title="The secret for {shown.key.name}">
        <span class="block">
          Configure the same name, algorithm and secret on the secondary. Then put the key on
          the transfer list under Settings; creating one here grants nothing on its own.
        </span>
        <code
          class="num mt-2 block rounded-sm border border-line bg-sunken px-3 py-2 text-[13px]
                 break-all text-ink select-all"
        >
          {shown.secret}
        </code>
        {#snippet actions()}
          <Button onclick={() => shown && copy(shown.secret)}>
            {copied ? "Copied" : "Copy"}
          </Button>
          <Button weight="quiet" onclick={() => ((shown = null), (copied = false))}>Hide</Button>
        {/snippet}
      </Notice>
    </div>
  {/if}

  <Table {columns} items={keys} key={(k) => k.id}>
    {#snippet row(key: TSIGKey)}
      <td class="py-1.5 pr-3 pl-5 {key.revokedAt ? 'text-ink-faint' : ''}">{key.name}</td>
      <td class="num px-3 py-1.5 text-ink-mute">{short(key.algorithm)}</td>
      <td
        class="num px-3 py-1.5 text-right text-[12px] text-ink-faint"
        title={exact(key.createdAt)}
      >
        {ago(key.createdAt)}
      </td>
      <td class="px-3 py-1.5">
        {#if key.revokedAt}
          <Chip tone="neutral">Withdrawn</Chip>
        {:else}
          <Chip tone="ok" dot>Signs</Chip>
        {/if}
      </td>
      <td class="py-1.5 pr-5 pl-3 text-right">
        {#if allowed && !key.revokedAt}
          <span class="flex items-center justify-end gap-1">
            <Button weight="quiet" onclick={() => reveal(key)}>Show secret</Button>
            <button
              type="button"
              onclick={() => ((revoking = key), (refused = null))}
              aria-label="Withdraw {key.name}"
              class="grid size-6 cursor-pointer place-items-center rounded-xs text-ink-faint
                     opacity-0 transition-opacity group-hover:opacity-100 hover:bg-crit-lo
                     hover:text-crit focus-visible:opacity-100"
            >
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" class="size-3.5">
                <circle cx="12" cy="12" r="9" />
                <path d="m6 6 12 12" stroke-linecap="round" />
              </svg>
            </button>
          </span>
        {/if}
      </td>
    {/snippet}

    {#snippet empty()}
      {#if loading}
        <p class="text-center text-[13px] text-ink-faint">Reading the keys…</p>
      {:else}
        <Empty title="No keys yet">
          A key lets a secondary pull a zone from any address, which an address list cannot do:
          it cannot tell two hosts behind one NAT apart, or authenticate a server somebody else
          runs.
          {#snippet actions()}
            {#if allowed}
              <Button weight="primary" onclick={() => (creating = true)}>Create one</Button>
            {/if}
          {/snippet}
        </Empty>
      {/if}
    {/snippet}
  </Table>
</div>

<Dialog bind:open={creating} title="New key">
  <form id="create-key" class="flex flex-col gap-4" onsubmit={create}>
    <Field
      label="Name"
      bind:value={newName}
      placeholder="secondary.example.com."
      autocomplete="off"
      hint="The name both ends agree on, in domain name syntax. It is what the secondary sends."
    />

    <fieldset class="flex flex-col gap-2">
      <legend class="sign mb-1 text-[11px] text-ink-faint">Algorithm</legend>
      {#each algorithms as algorithm (algorithm.value)}
        <label
          class="flex cursor-pointer items-start gap-2.5 rounded-sm border px-3 py-2
                 transition-colors {newAlgorithm === algorithm.value
            ? 'border-signal bg-signal-lo'
            : 'border-line bg-surface hover:bg-raised'}"
        >
          <input
            type="radio"
            name="algorithm"
            value={algorithm.value}
            bind:group={newAlgorithm}
            class="mt-1 accent-[var(--signal)]"
          />
          <span class="flex flex-col gap-0.5">
            <span class="num text-[12px] {newAlgorithm === algorithm.value ? 'text-signal' : ''}">
              {algorithm.label}
            </span>
            <span class="text-[12px] text-ink-mute">{algorithm.what}</span>
          </span>
        </label>
      {/each}
    </fieldset>

    <Field
      label="Secret"
      bind:value={newSecret}
      placeholder="generated if left empty"
      autocomplete="off"
      hint="Base64. Leave it empty and one is generated, long enough for the algorithm. Fill it in to match a secondary that already has a key."
    />

    {#if refused}
      <p
        class="rounded-sm border border-line border-l-2 border-l-crit bg-raised px-3 py-2
               text-[13px] text-ink-mute"
        role="alert"
      >
        {refused}
      </p>
    {/if}
  </form>

  {#snippet actions()}
    <Button weight="quiet" onclick={() => (creating = false)}>Cancel</Button>
    <Button weight="primary" type="submit" form="create-key" disabled={working || !newName.trim()}>
      {working ? "Creating…" : "Create key"}
    </Button>
  {/snippet}
</Dialog>

<Dialog
  open={revoking !== null}
  onclose={() => (revoking = null)}
  title="Withdraw {revoking?.name ?? ''}"
>
  <p class="text-[13px] text-ink-mute">
    It stops signing at once, and a secondary still configured with it starts being refused.
    Its secret is cleared: a token leaves only a hash behind when it is revoked, and a key
    would leave material nothing will read again.
  </p>
  <p class="text-[13px] text-ink-mute">
    The name and the dates stay in this list, and the name is free for a replacement, so
    rotating a key does not mean renaming it on the other end.
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
    <Button weight="quiet" onclick={() => (revoking = null)}>Cancel</Button>
    <Button
      weight="primary"
      onclick={revoke}
      disabled={working}
      class="border-crit bg-crit text-ground hover:border-crit hover:bg-crit"
    >
      {working ? "Withdrawing…" : "Withdraw it"}
    </Button>
  {/snippet}
</Dialog>
