<script lang="ts">
  /**
   * The identity providers people sign in through, and who each lets in
   * (docs/decisions/d51-accounts-come-from-an-identity-provider.md).
   *
   * A list and nothing else: a provider has a dozen settings, and they are
   * edited on a page of their own rather than squeezed into a dialog here.
   */
  import { asked } from "#lib/asked.ts";
  import { goto } from "$app/navigation";
  import { api, ApiError, NetworkError } from "#lib/api/index.ts";
  import type { IdentityProvider, Scope } from "#lib/api/index.ts";
  import { session } from "#lib/session.svelte.ts";
  import Bar from "#lib/components/Bar.svelte";
  import Button from "#lib/components/Button.svelte";
  import Chip from "#lib/components/Chip.svelte";
  import Dialog from "#lib/components/Dialog.svelte";
  import Empty from "#lib/components/Empty.svelte";
  import Notice from "#lib/components/Notice.svelte";
  import Table from "#lib/components/Table.svelte";
  import type { Column } from "#lib/components/Table.svelte";

  let providers = $state<IdentityProvider[]>([]);
  let loading = $state(true);
  let trouble = $state<string | null>(null);

  const columns: Column[] = [
    { label: "Name" },
    { label: "Kind", width: "6rem" },
    { label: "Address", narrow: false },
    { label: "Lets in", width: "15rem" },
    { label: "State", width: "8rem" },
    { label: "", width: "9rem" },
  ];

  /** The provider the removal dialog is about. */
  let removing = $state<IdentityProvider | null>(null);
  let working = $state(false);
  let refused = $state<string | null>(null);

  async function remove() {
    if (!removing) return;
    working = true;
    refused = null;
    try {
      await api.delete("/identity-providers/{providerId}", { path: { providerId: removing.id } });
      removing = null;
      await load();
    } catch (err) {
      refused =
        err instanceof ApiError ? (err.detail ?? err.title) : "The provider was not removed.";
    } finally {
      working = false;
    }
  }

  const allowed = $derived(session.can("admin"));

  async function load() {
    loading = true;
    trouble = null;
    try {
      providers = await api.get("/identity-providers");
    } catch (err) {
      providers = [];
      trouble =
        err instanceof NetworkError
          ? "The server did not answer."
          : err instanceof ApiError
            ? (err.detail ?? err.title)
            : "The providers could not be read.";
    } finally {
      loading = false;
    }
  }

  // Listing needs the admin scope, so a session without it asks for nothing
  // and is told once why.
  $effect(() => {
    if (allowed) load();
  });

  // The command palette asks for a new one with /providers?new.
  $effect(() => {
    if (asked("new") && allowed) void goto("/providers/new");
  });

  const order: Scope[] = ["admin", "write", "read"];

  /** How many groups each scope is given, strongest first, the empty ones left out. */
  function lets(p: IdentityProvider): { scope: Scope; count: number }[] {
    return order
      .map((scope) => ({ scope, count: p.mappings.filter((m) => m.scope === scope).length }))
      .filter((s) => s.count > 0);
  }

  const address = (p: IdentityProvider) => p.oidc?.issuer ?? p.ldap?.url ?? "";
</script>

<svelte:head><title>Providers — Wegweiser</title></svelte:head>

<Bar title="Identity providers">
  {#snippet actions()}
    {#if allowed}
      <Button onclick={() => goto("/providers/new?kind=ldap")}>+ Directory</Button>
      <Button weight="primary" onclick={() => goto("/providers/new?kind=oidc")}>
        + OpenID Connect
      </Button>
    {/if}
  {/snippet}
</Bar>

<div class="flex flex-1 flex-col overflow-auto">
  {#if !allowed}
    <div class="px-5 pt-4">
      <Notice tone="warn" title="This session may not manage providers">
        A provider decides who may sign in and with what, so seeing or changing one needs the
        admin scope.
      </Notice>
    </div>
  {/if}

  {#if trouble}
    <div class="px-5 pt-4">
      <Notice tone="crit" title="The providers could not be listed">
        {trouble}
        {#snippet actions()}
          <Button onclick={load}>Try again</Button>
        {/snippet}
      </Notice>
    </div>
  {/if}

  {#if allowed}
    <Table {columns} items={providers} key={(p) => p.id}>
      {#snippet row(p: IdentityProvider)}
        <td class="py-1.5 pr-3 pl-5">
          <a
            href="/providers/{p.id}"
            class="hover:text-signal {p.enabled ? '' : 'text-ink-faint'}">{p.name}</a
          >
        </td>
        <td class="px-3 py-1.5">
          <Chip tone="info">{p.kind === "oidc" ? "OIDC" : "LDAP"}</Chip>
        </td>
        <td class="num max-w-0 truncate px-3 py-1.5 text-ink-mute" title={address(p)}>
          {address(p)}
        </td>
        <td class="px-3 py-1.5">
          {#if lets(p).length === 0}
            <Chip tone="warn" title="No group is given a scope, so every sign-in is refused">
              Nobody yet
            </Chip>
          {:else}
            <span class="num flex gap-3 text-[12px] text-ink-mute">
              {#each lets(p) as s (s.scope)}
                <span><span class="text-ink">{s.count}</span> {s.scope}</span>
              {/each}
            </span>
          {/if}
        </td>
        <td class="px-3 py-1.5">
          {#if p.enabled}
            <Chip tone="ok" dot>Offered</Chip>
          {:else}
            <Chip>Off</Chip>
          {/if}
        </td>
        <td class="py-1 pr-5 pl-3 text-right">
          <span class="flex items-center justify-end gap-1">
            <a
              href="/providers/{p.id}"
              aria-label="Edit {p.name}"
              class="sign inline-flex h-7 items-center rounded-sm px-2.5 text-[12px] text-ink-mute
                     transition-colors hover:bg-raised hover:text-ink">Edit</a
            >
            <button
              type="button"
              onclick={() => ((removing = p), (refused = null))}
              aria-label="Remove {p.name}"
              class="sign inline-flex h-7 cursor-pointer items-center rounded-sm px-2.5 text-[12px]
                     text-ink-mute transition-colors hover:bg-crit-lo hover:text-crit"
            >
              Remove
            </button>
          </span>
        </td>
      {/snippet}

      {#snippet empty()}
        {#if loading}
          <p class="text-center text-[13px] text-ink-faint">Reading the providers…</p>
        {:else}
          <Empty title="No providers yet">
            People sign in with the accounts they already have, in Authentik, Keycloak, Entra,
            FreeIPA or Active Directory, and their groups decide what they may do. Tokens go on
            working beside them.
            {#snippet actions()}
              <Button onclick={() => goto("/providers/new?kind=ldap")}>Add a directory</Button>
              <Button weight="primary" onclick={() => goto("/providers/new?kind=oidc")}>
                Add OpenID Connect
              </Button>
            {/snippet}
          </Empty>
        {/if}
      {/snippet}
    </Table>
  {/if}
</div>

<Dialog open={removing !== null} onclose={() => (removing = null)} title="Remove {removing?.name ?? ''}">
  <p class="text-[13px] text-ink-mute">
    Nobody signs in through it any more, and everybody who did is signed out at once, on every
    member. The commits they made keep their names. Switching it off instead keeps the settings.
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
    <Button weight="quiet" onclick={() => (removing = null)}>Cancel</Button>
    <Button
      weight="primary"
      onclick={remove}
      disabled={working}
      class="border-crit bg-crit text-ground hover:border-crit hover:bg-crit"
    >
      {working ? "Removing…" : "Remove it"}
    </Button>
  {/snippet}
</Dialog>
