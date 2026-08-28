<script lang="ts">
  /**
   * The credentials that may use this API.
   *
   * The secret is shown once, when it is created, and never again, not
   * because showing it twice would be inconvenient but because the server does
   * not have it: what is stored is a SHA-256, so a copy of the database is not
   * a copy of the credentials (docs/decisions/ D5).
   */
  import { api, ApiError, NetworkError } from "$lib/api";
  import type { Scope, Token } from "$lib/api";
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

  const scopes: { value: Scope; what: string }[] = [
    { value: "read", what: "Read zones, records and history." },
    { value: "write", what: "Everything read allows, and change what is here." },
    { value: "admin", what: "Everything write allows, and manage these tokens." },
  ];

  let tokens = $state<Token[]>([]);
  let loading = $state(true);
  let trouble = $state<string | null>(null);

  let creating = $state(false);
  let newName = $state("");
  let newScope = $state<Scope>("write");
  let working = $state(false);
  let refused = $state<string | null>(null);

  /** The one time the secret exists outside the server. */
  let minted = $state<{ token: Token; secret: string } | null>(null);
  let copied = $state(false);

  let revoking = $state<Token | null>(null);

  const columns: Column[] = [
    { label: "Name" },
    { label: "Prefix", width: "12rem" },
    { label: "Allowed", width: "8rem" },
    { label: "Created", align: "right", width: "9rem" },
    { label: "Last used", align: "right", width: "9rem" },
    { label: "State", width: "9rem" },
    { label: "", width: "6rem" },
  ];

  const allowed = $derived(session.can("admin"));

  async function load() {
    loading = true;
    trouble = null;
    try {
      tokens = await api.get("/tokens");
    } catch (err) {
      tokens = [];
      trouble =
        err instanceof NetworkError
          ? "The server did not answer."
          : err instanceof ApiError
            ? (err.detail ?? err.title)
            : "The tokens could not be read.";
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
      const result = await api.post("/tokens", {
        body: { name: newName.trim(), scopes: [newScope] },
      });
      creating = false;
      newName = "";
      copied = false;
      minted = { token: result.token, secret: result.secret };
      await load();
    } catch (err) {
      refused =
        err instanceof ApiError ? (err.detail ?? err.title) : "The token could not be created.";
    } finally {
      working = false;
    }
  }

  async function revoke() {
    if (!revoking) return;
    working = true;
    refused = null;
    try {
      await api.delete("/tokens/{tokenId}", { path: { tokenId: revoking.id } });
      revoking = null;
      await load();
    } catch (err) {
      refused =
        err instanceof ApiError ? (err.detail ?? err.title) : "The token could not be revoked.";
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

  function condition(token: Token): {
    label: string;
    tone: "ok" | "warn" | "crit" | "neutral";
  } {
    if (token.revokedAt) return { label: "Revoked", tone: "neutral" };
    if (token.expiresAt && new Date(token.expiresAt) <= new Date()) {
      return { label: "Expired", tone: "warn" };
    }
    return { label: "Usable", tone: "ok" };
  }
</script>

<svelte:head><title>Tokens — Wegweiser</title></svelte:head>

<Bar title="Tokens">
  {#snippet actions()}
    {#if allowed}
      <Button weight="primary" onclick={() => ((creating = true), (refused = null))}>
        + New token
      </Button>
    {/if}
  {/snippet}
</Bar>

<div class="flex flex-1 flex-col overflow-auto">
  {#if !allowed}
    <div class="px-5 pt-4">
      <Notice tone="warn" title="This session may not manage tokens">
        Managing credentials needs the admin scope, and this session carries
        <span class="num text-ink">{session.who?.scopes?.at(-1) ?? "none"}</span>.
      </Notice>
    </div>
  {/if}

  {#if trouble}
    <div class="px-5 pt-4">
      <Notice tone="crit" title="The tokens could not be listed">
        {trouble}
        {#snippet actions()}
          <Button onclick={load}>Try again</Button>
        {/snippet}
      </Notice>
    </div>
  {/if}

  {#if minted}
    <div class="px-5 pt-4">
      <Notice tone="signal" title="Copy it now; it is not shown again">
        <span class="block">
          This is the only time <span class="num text-ink">{minted.token.name}</span> exists
          outside the server. What is stored is a SHA-256 of it, so nobody can read it back —
          not this interface, and not somebody holding a copy of the database.
        </span>
        <code
          class="num mt-2 block rounded-sm border border-line bg-sunken px-3 py-2 text-[13px]
                 break-all text-ink select-all"
        >
          {minted.secret}
        </code>
        {#snippet actions()}
          <Button onclick={() => minted && copy(minted.secret)}>
            {copied ? "Copied" : "Copy"}
          </Button>
          <Button weight="quiet" onclick={() => ((minted = null), (copied = false))}>Done</Button>
        {/snippet}
      </Notice>
    </div>
  {/if}

  <Table {columns} items={tokens} key={(t) => t.id}>
    {#snippet row(token: Token)}
      {@const shown = condition(token)}
      <td class="py-1.5 pr-3 pl-5 {token.revokedAt ? 'text-ink-faint' : ''}">
        {token.name}
        {#if session.who?.name === token.name}
          <span class="sign ml-1.5 text-[10px] text-signal">this session</span>
        {/if}
      </td>
      <td class="num px-3 py-1.5 text-ink-mute">{token.prefix}…</td>
      <td class="px-3 py-1.5">
        <Chip tone={token.scopes.includes("admin") ? "signal" : "neutral"}>
          {token.scopes.at(-1)}
        </Chip>
      </td>
      <td
        class="num px-3 py-1.5 text-right text-[12px] text-ink-faint"
        title={exact(token.createdAt)}
      >
        {ago(token.createdAt)}
      </td>
      <td
        class="num px-3 py-1.5 text-right text-[12px] text-ink-faint"
        title={exact(token.lastUsedAt)}
      >
        {token.lastUsedAt ? ago(token.lastUsedAt) : "never"}
      </td>
      <td class="px-3 py-1.5">
        <Chip tone={shown.tone} dot={shown.tone === "ok"}>{shown.label}</Chip>
      </td>
      <td class="py-1.5 pr-5 pl-3 text-right">
        {#if allowed && !token.revokedAt}
          <button
            type="button"
            onclick={() => ((revoking = token), (refused = null))}
            aria-label="Revoke {token.name}"
            class="grid size-6 cursor-pointer place-items-center rounded-xs text-ink-faint
                   opacity-0 transition-opacity group-hover:opacity-100 hover:bg-crit-lo
                   hover:text-crit focus-visible:opacity-100"
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" class="size-3.5">
              <circle cx="12" cy="12" r="9" />
              <path d="m6 6 12 12" stroke-linecap="round" />
            </svg>
          </button>
        {/if}
      </td>
    {/snippet}

    {#snippet empty()}
      {#if loading}
        <p class="text-center text-[13px] text-ink-faint">Reading the tokens…</p>
      {:else}
        <Empty title="No tokens yet">
          A token is how the command line and anything else authenticates. The secret is shown
          once, when it is created, and never again.
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

<Dialog bind:open={creating} title="New token">
  <form id="create-token" class="flex flex-col gap-4" onsubmit={create}>
    <Field
      label="Name"
      bind:value={newName}
      placeholder="deploy pipeline"
      autocomplete="off"
      hint="What this token is for, as a person would say it. It is what the history points at."
    />

    <fieldset class="flex flex-col gap-2">
      <legend class="sign mb-1 text-[11px] text-ink-faint">Allowed</legend>
      {#each scopes as scope (scope.value)}
        <label
          class="flex cursor-pointer items-start gap-2.5 rounded-sm border px-3 py-2
                 transition-colors {newScope === scope.value
            ? 'border-signal bg-signal-lo'
            : 'border-line bg-surface hover:bg-raised'}"
        >
          <input
            type="radio"
            name="scope"
            value={scope.value}
            bind:group={newScope}
            class="mt-1 accent-[var(--signal)]"
          />
          <span class="flex flex-col gap-0.5">
            <span class="sign text-[12px] {newScope === scope.value ? 'text-signal' : ''}">
              {scope.value}
            </span>
            <span class="text-[12px] text-ink-mute">{scope.what}</span>
          </span>
        </label>
      {/each}
    </fieldset>

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
    <Button
      weight="primary"
      type="submit"
      form="create-token"
      disabled={working || !newName.trim()}
    >
      {working ? "Creating…" : "Create token"}
    </Button>
  {/snippet}
</Dialog>

<Dialog
  open={revoking !== null}
  onclose={() => (revoking = null)}
  title="Revoke {revoking?.name ?? ''}"
>
  <p class="text-[13px] text-ink-mute">
    It stops working at once, and anything using it starts being refused. It stays in this
    list: the history points at a token to say who did something, and a name that has been
    erased answers nothing.
  </p>
  {#if session.who?.name === revoking?.name}
    <Notice tone="warn" title="This is the token you are signed in with">
      Revoking it ends this session, and the next request the interface makes is refused.
    </Notice>
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
    <Button weight="quiet" onclick={() => (revoking = null)}>Cancel</Button>
    <Button
      weight="primary"
      onclick={revoke}
      disabled={working}
      class="border-crit bg-crit text-ground hover:border-crit hover:bg-crit"
    >
      {working ? "Revoking…" : "Revoke it"}
    </Button>
  {/snippet}
</Dialog>
