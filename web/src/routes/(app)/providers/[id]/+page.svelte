<script lang="ts">
  /**
   * One identity provider: who it lets in, and how this server reaches it.
   * `new` in place of an identifier makes one, of the kind ?kind names.
   *
   * Who it lets in comes first, grouped by scope from the strongest down,
   * because that is the decision a provider is for; the connection is
   * plumbing that is set once (docs/decisions/d51-accounts-come-from-an-identity-provider.md).
   */
  import { untrack } from "svelte";

  import { goto } from "$app/navigation";
  import { page } from "$app/state";
  import { asked } from "#lib/asked.ts";
  import { api, ApiError, NetworkError } from "#lib/api/index.ts";
  import type {
    CheckStep,
    IdentityProvider,
    IdentityProviderCheck,
    IdentityProviderInput,
    Scope,
  } from "#lib/api/index.ts";
  import { session } from "#lib/session.svelte.ts";
  import Bar from "#lib/components/Bar.svelte";
  import Button from "#lib/components/Button.svelte";
  import Chip from "#lib/components/Chip.svelte";
  import Dialog from "#lib/components/Dialog.svelte";
  import Field from "#lib/components/Field.svelte";
  import ListEditor from "#lib/components/ListEditor.svelte";
  import Notice from "#lib/components/Notice.svelte";
  import Panel from "#lib/components/Panel.svelte";

  type Kind = "oidc" | "ldap";

  /** Draft is a provider as the fields hold it. Secrets start empty and stay so unless typed. */
  interface Draft {
    kind: Kind;
    name: string;
    enabled: boolean;
    groups: Record<Scope, string[]>;
    caCertificate: string;
    oidc: {
      issuer: string;
      clientId: string;
      clientSecret: string;
      redirectUris: string[];
      scopes: string;
      groupsClaim: string;
      nameClaim: string;
    };
    ldap: {
      url: string;
      startTls: boolean;
      bindDn: string;
      bindPassword: string;
      userBase: string;
      userFilter: string;
      nameAttribute: string;
      groups: "memberOf" | "search";
      groupBase: string;
      groupFilter: string;
    };
  }

  /** The scopes, strongest first, and what each lets a person do (D5). */
  const scopes: { scope: Scope; what: string }[] = [
    {
      scope: "admin",
      what: "Everything write may, and who may: tokens, transfer keys, these providers and the cluster's members.",
    },
    { scope: "write", what: "Every change to zones, records and settings." },
    { scope: "read", what: "Sees everything, the stream and the history included, and changes nothing." },
  ];

  const kinds: Kind[] = ["oidc", "ldap"];

  /** Where a directory's groups are read from, and which directories want which. */
  const groupSources = [
    {
      value: "memberOf",
      label: "The person's memberOf attribute",
      what: "FreeIPA, Active Directory, and OpenLDAP with the memberof overlay.",
    },
    {
      value: "search",
      label: "A search for groups naming them",
      what: "Directories without memberOf, or Active Directory's nested groups.",
    },
  ] as const;

  /** A new provider, with the defaults the server would fill in. */
  function blank(kind: Kind): Draft {
    return {
      kind,
      name: "",
      enabled: true,
      groups: { admin: [], write: [], read: [] },
      caCertificate: "",
      oidc: {
        issuer: "",
        clientId: "",
        clientSecret: "",
        redirectUris: [],
        scopes: "openid profile email groups",
        groupsClaim: "groups",
        nameClaim: "preferred_username",
      },
      ldap: {
        url: "",
        startTls: false,
        bindDn: "",
        bindPassword: "",
        userBase: "",
        userFilter: "(uid={username})",
        nameAttribute: "uid",
        groups: "memberOf",
        groupBase: "",
        groupFilter: "(member={dn})",
      },
    };
  }

  function toDraft(p: IdentityProvider): Draft {
    const d = blank(p.kind);
    d.name = p.name;
    d.enabled = p.enabled;
    d.caCertificate = p.caCertificate ?? "";
    for (const m of p.mappings) d.groups[m.scope].push(m.group);
    if (p.oidc) {
      d.oidc = {
        ...d.oidc,
        issuer: p.oidc.issuer,
        clientId: p.oidc.clientId,
        redirectUris: [...p.oidc.redirectUris],
        scopes: p.oidc.scopes.join(" "),
        groupsClaim: p.oidc.groupsClaim,
        nameClaim: p.oidc.nameClaim,
      };
    }
    if (p.ldap) {
      d.ldap = {
        ...d.ldap,
        url: p.ldap.url,
        startTls: p.ldap.startTls,
        bindDn: p.ldap.bindDn ?? "",
        userBase: p.ldap.userBase,
        userFilter: p.ldap.userFilter,
        nameAttribute: p.ldap.nameAttribute,
        groups: p.ldap.groups,
        groupBase: p.ldap.groupBase ?? "",
        groupFilter: p.ldap.groupFilter ?? d.ldap.groupFilter,
      };
    }
    return d;
  }

  /**
   * toInput is the provider as the API takes it. A secret left empty is left
   * out, which keeps the one the server holds.
   */
  function toInput(d: Draft): IdentityProviderInput {
    const input: IdentityProviderInput = {
      name: d.name.trim(),
      kind: d.kind,
      enabled: d.enabled,
      mappings: scopes.flatMap(({ scope }) => d.groups[scope].map((group) => ({ group, scope }))),
      caCertificate: d.caCertificate.trim() || undefined,
    };
    if (d.kind === "oidc") {
      const o = d.oidc;
      input.oidc = {
        issuer: o.issuer.trim(),
        clientId: o.clientId.trim(),
        clientSecret: o.clientSecret || undefined,
        redirectUris: o.redirectUris,
        scopes: o.scopes.split(/[\s,]+/).filter(Boolean),
        groupsClaim: o.groupsClaim.trim(),
        nameClaim: o.nameClaim.trim(),
      };
    } else {
      const l = d.ldap;
      const search = l.groups === "search";
      input.ldap = {
        url: l.url.trim(),
        startTls: l.startTls,
        bindDn: l.bindDn.trim() || undefined,
        bindPassword: l.bindPassword || undefined,
        userBase: l.userBase.trim(),
        userFilter: l.userFilter.trim(),
        nameAttribute: l.nameAttribute.trim(),
        groups: l.groups,
        groupBase: search ? l.groupBase.trim() || undefined : undefined,
        groupFilter: search ? l.groupFilter.trim() || undefined : undefined,
      };
    }
    return input;
  }

  const id = $derived(page.params.id ?? "");
  const creating = $derived(id === "new");
  const allowed = $derived(session.can("admin"));

  let stored = $state<IdentityProvider | null>(null);
  let draft = $state<Draft>(blank("oidc"));
  let loading = $state(true);
  let trouble = $state<string | null>(null);
  let saving = $state(false);
  let refused = $state<string | null>(null);
  let said = $state<string | null>(null);
  let removing = $state(false);

  /** The last check of the provider as it is saved, and whom it looked up. */
  let checked = $state<IdentityProviderCheck | null>(null);
  let checking = $state(false);
  let lookup = $state("");

  async function load(which: string) {
    trouble = null;
    refused = null;
    checked = null;
    if (which === "new") {
      stored = null;
      draft = blank(page.url.searchParams.get("kind") === "ldap" ? "ldap" : "oidc");
      loading = false;
      return;
    }
    loading = true;
    try {
      stored = await api.get("/identity-providers/{providerId}", { path: { providerId: which } });
      draft = toDraft(stored);
    } catch (err) {
      stored = null;
      trouble =
        err instanceof NetworkError
          ? "The server did not answer."
          : err instanceof ApiError
            ? (err.detail ?? err.title)
            : "The provider could not be read.";
    } finally {
      loading = false;
    }
  }

  // Again whenever the address names another provider, and only then: the
  // draft is the person's until they save it or leave.
  $effect(() => {
    const which = id;
    if (allowed) untrack(() => load(which));
  });

  // Said once after a provider is made, which navigates here with ?added.
  $effect(() => {
    if (asked("added")) said = "Added. It is offered on the sign-in page while it is switched on.";
  });

  const dirty = $derived(
    creating || (stored !== null && JSON.stringify(draft) !== JSON.stringify(toDraft(stored))),
  );
  const nobody = $derived(scopes.every(({ scope }) => draft.groups[scope].length === 0));

  /** The callback on the address this page is served from, which is the one most often missing. */
  const callback = $derived(`${page.url.origin}/api/v1/auth/oidc/callback`);
  const callbackListed = $derived(
    draft.oidc.redirectUris.some((u) => u.toLowerCase() === callback.toLowerCase()),
  );

  async function save(event: SubmitEvent) {
    event.preventDefault();
    saving = true;
    refused = null;
    said = null;
    try {
      if (creating) {
        const made = await api.post("/identity-providers", { body: toInput(draft) });
        await goto(`/providers/${made.id}?added`, { replaceState: true });
      } else if (stored) {
        stored = await api.put("/identity-providers/{providerId}", {
          path: { providerId: stored.id },
          body: toInput(draft),
        });
        draft = toDraft(stored);
        // A check describes what was saved before; it says nothing about this.
        checked = null;
        said = "Saved. A session already open keeps the scope it was given until it ends.";
      }
    } catch (err) {
      refused =
        err instanceof ApiError ? (err.detail ?? err.title) : "The provider was not saved.";
    } finally {
      saving = false;
    }
  }

  async function check() {
    if (!stored) return;
    checking = true;
    refused = null;
    checked = null;
    try {
      const username = draft.kind === "ldap" ? lookup.trim() : "";
      checked = await api.post("/identity-providers/{providerId}/check", {
        path: { providerId: stored.id },
        body: username ? { username } : {},
      });
    } catch (err) {
      refused =
        err instanceof ApiError ? (err.detail ?? err.title) : "The provider could not be checked.";
    } finally {
      checking = false;
    }
  }

  const stepTone = (s: CheckStep) =>
    s.status === "ok" ? "ok" : s.status === "failed" ? "crit" : "warn";
  const stepWord = (s: CheckStep) =>
    s.status === "ok" ? "OK" : s.status === "failed" ? "Failed" : "Unknown";

  async function remove() {
    if (!stored) return;
    saving = true;
    refused = null;
    try {
      await api.delete("/identity-providers/{providerId}", { path: { providerId: stored.id } });
      removing = false;
      await goto("/providers");
    } catch (err) {
      refused =
        err instanceof ApiError ? (err.detail ?? err.title) : "The provider was not removed.";
    } finally {
      saving = false;
    }
  }

  const kindName = (k: Kind) => (k === "oidc" ? "OpenID Connect" : "directory");
  const areaClass =
    "num w-full rounded-sm border border-line bg-surface px-3 py-2 text-[12px] text-ink " +
    "transition-colors outline-none placeholder:text-ink-faint focus:border-signal";
</script>

<svelte:head>
  <title>{creating ? "New provider" : (stored?.name ?? "Provider")} — Wegweiser</title>
</svelte:head>

<Bar
  title="Identity providers"
  subject={creating ? `new ${kindName(draft.kind)}` : (stored?.name ?? "")}
>
  {#snippet actions()}
    <Button weight="quiet" onclick={() => goto("/providers")}>Back</Button>
    {#if allowed && (creating || stored)}
      <Button weight="primary" type="submit" form="provider" disabled={saving || !dirty}>
        {saving ? "Saving…" : creating ? "Add provider" : "Save"}
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
  {:else if trouble}
    <div class="px-5 pt-4">
      <Notice tone="crit" title="The provider could not be read">
        {trouble}
        {#snippet actions()}
          <Button onclick={() => load(id)}>Try again</Button>
        {/snippet}
      </Notice>
    </div>
  {:else if loading}
    <p class="px-5 pt-6 text-[13px] text-ink-faint">Reading the provider…</p>
  {:else}
    <form id="provider" class="flex flex-col gap-6 px-5 py-6" onsubmit={save}>
      {#if refused}
        <div class="max-w-3xl">
          <Notice tone="crit" title="That did not work">{refused}</Notice>
        </div>
      {/if}
      {#if said}
        <div class="max-w-3xl">
          <Notice tone="signal" title="Done">{said}</Notice>
        </div>
      {/if}

      {#if creating}
        <div class="flex gap-1.5" role="radiogroup" aria-label="Kind">
          {#each kinds as k (k)}
            <button
              type="button"
              role="radio"
              aria-checked={draft.kind === k}
              onclick={() => (draft.kind = k)}
              class="sign h-8 cursor-pointer rounded-sm border px-3 text-[13px] transition-colors
                     {draft.kind === k
                ? 'border-signal bg-signal-lo text-signal'
                : 'border-line bg-surface text-ink-mute hover:bg-raised'}"
            >
              {k === "oidc" ? "OpenID Connect" : "LDAP directory"}
            </button>
          {/each}
        </div>
      {/if}

      {#if stored}
        <Panel heading="Check it">
          {#snippet blurb()}
            Tries what a sign-in needs, from this member and short of somebody signing in. It
            checks the provider as it is saved.
          {/snippet}

          <div class="flex flex-wrap items-end gap-3">
            {#if draft.kind === "ldap"}
              <div class="min-w-56 flex-1">
                <Field
                  label="Look somebody up"
                  bind:value={lookup}
                  placeholder="alice"
                  autocomplete="off"
                  spellcheck={false}
                  hint="Optional. Who they are, their groups and their scope, without a password."
                  onkeydown={(e) => {
                    if (e.key === "Enter") {
                      e.preventDefault();
                      void check();
                    }
                  }}
                />
              </div>
            {/if}
            <Button type="button" onclick={check} disabled={checking || dirty}>
              {checking ? "Checking…" : "Check"}
            </Button>
            {#if dirty}
              <span class="pb-2 text-xs text-ink-faint">Save first: the check tries what is saved.</span>
            {/if}
          </div>

          {#if checked}
            <p class="text-[13px] {checked.ok ? 'text-ok' : 'text-crit'}" role="status">
              {checked.ok
                ? "It works as far as can be told from here."
                : "It does not work as it stands."}
            </p>
            <ol class="flex flex-col gap-2.5" aria-label="What was tried">
              {#each checked.steps as step, i (i)}
                <li class="flex items-start gap-3">
                  <span class="w-16 shrink-0 pt-px"><Chip tone={stepTone(step)}>{stepWord(step)}</Chip></span>
                  <span class="flex min-w-0 flex-col gap-0.5">
                    <span class="text-[13px] text-ink">{step.step}</span>
                    <span class="text-[12px] break-words text-ink-mute">{step.detail}</span>
                  </span>
                </li>
              {/each}
            </ol>
            {#if checked.person}
              <div class="flex flex-col gap-1.5 rounded-sm border border-line bg-raised px-3 py-2.5">
                <p class="flex flex-wrap items-center gap-2 text-[13px]">
                  <span class="text-ink">{checked.person.name}</span>
                  {#if checked.person.scope}
                    <Chip tone="signal">{checked.person.scope}</Chip>
                  {:else}
                    <Chip tone="crit">no scope</Chip>
                  {/if}
                </p>
                <p class="num text-[12px] break-all text-ink-mute">{checked.person.dn}</p>
                {#if checked.person.groups.length > 0}
                  <ul class="num flex flex-col text-[12px] text-ink-mute" aria-label="Their groups">
                    {#each checked.person.groups as group (group)}
                      <li class="break-all">{group}</li>
                    {/each}
                  </ul>
                {:else}
                  <p class="text-[12px] text-ink-faint">In no group.</p>
                {/if}
              </div>
            {/if}
          {/if}
        </Panel>
      {/if}

      <Panel heading="Who it lets in">
        {#snippet blurb()}
          A person holds the strongest scope any of their groups is given here. A person in none
          of these groups is refused, however well they signed in.
        {/snippet}

        {#if nobody}
          <Notice tone="warn" title="Nobody can sign in through it yet">
            Give at least one group a scope. Until then every sign-in through this provider is
            refused.
          </Notice>
        {/if}

        {#each scopes as { scope, what } (scope)}
          <div class="flex flex-col gap-2 border-b border-line pb-4 last:border-b-0 last:pb-0">
            <div class="flex flex-wrap items-baseline gap-2">
              <Chip tone={scope === "admin" ? "signal" : scope === "write" ? "info" : "neutral"}>
                {scope}
              </Chip>
              <span class="text-[12px] text-ink-mute">{what}</span>
            </div>
            <ListEditor
              label="Groups given {scope}"
              bind:items={draft.groups[scope]}
              placeholder={draft.kind === "oidc" ? `dns-${scope}` : `cn=dns-${scope},cn=groups,…`}
              empty="No group is given {scope}."
            />
          </div>
        {/each}

        <p class="text-[12px] text-ink-faint">
          {#if draft.kind === "oidc"}
            A group is a value of the {draft.oidc.groupsClaim || "groups"} claim, as the provider
            sends it.
          {:else}
            A group is its DN, or the value of its first RDN: <span class="num">dns-admin</span>
            matches <span class="num">cn=dns-admin,cn=groups,…</span>.
          {/if}
          Case does not matter.
        </p>
      </Panel>

      <Panel heading={draft.kind === "oidc" ? "The provider" : "The directory"}>
        {#snippet blurb()}
          {#if draft.kind === "oidc"}
            Signing in uses the authorization code flow with PKCE. Register this server with the
            provider as a confidential client, then copy what it says here.
          {:else}
            A person's entry is found with the filter, and their password checked by binding as
            it, over LDAPS or StartTLS. The password is passed on and forgotten.
          {/if}
        {/snippet}

        <div class="grid gap-4 sm:grid-cols-2">
          <Field
            label="Name"
            bind:value={draft.name}
            placeholder={draft.kind === "oidc" ? "Authentik" : "FreeIPA"}
            autocomplete="off"
            hint="What the sign-in page offers it as."
          />
          <label class="flex items-center gap-2 self-center text-[13px] text-ink-mute sm:pt-4">
            <input type="checkbox" bind:checked={draft.enabled} class="size-3.5 accent-signal" />
            Offered on the sign-in page
          </label>
        </div>

        {#if draft.kind === "oidc"}
          <Field
            label="Issuer"
            bind:value={draft.oidc.issuer}
            placeholder="https://auth.example.com/application/o/wegweiser/"
            spellcheck={false}
            autocomplete="off"
            hint="The discovery document is read from below it."
          />
          <div class="grid gap-4 sm:grid-cols-2">
            <Field
              label="Client ID"
              bind:value={draft.oidc.clientId}
              spellcheck={false}
              autocomplete="off"
            />
            <Field
              label="Client secret"
              type="password"
              bind:value={draft.oidc.clientSecret}
              autocomplete="new-password"
              placeholder={creating ? "" : "kept; type a new one to replace it"}
              hint="Never shown again once saved."
            />
          </div>

          <div class="flex flex-col gap-1.5">
            <span class="sign text-[11px] text-ink-faint">Redirect URIs</span>
            <ListEditor
              label="Redirect URIs"
              bind:items={draft.oidc.redirectUris}
              placeholder="https://ns1.example.com/api/v1/auth/oidc/callback"
              empty="None yet. A sign-in returns only to one of these."
            />
            {#if !callbackListed}
              <p class="flex flex-wrap items-center gap-x-2 text-xs text-ink-mute">
                <span>
                  A sign-in started on this page returns to
                  <span class="num break-all text-ink">{callback}</span>, which is not listed.
                </span>
                <Button
                  type="button"
                  weight="quiet"
                  onclick={() => (draft.oidc.redirectUris = [...draft.oidc.redirectUris, callback])}
                >
                  Add it
                </Button>
              </p>
            {/if}
            <p class="text-xs text-ink-mute">
              One for each address a member is reached by, each registered with the provider too.
            </p>
          </div>

          <div class="grid gap-4 sm:grid-cols-3">
            <Field
              label="Scopes"
              bind:value={draft.oidc.scopes}
              spellcheck={false}
              hint="Asked for at sign-in. openid is always among them."
            />
            <Field
              label="Groups claim"
              bind:value={draft.oidc.groupsClaim}
              spellcheck={false}
              hint="Where a person's groups are read from."
            />
            <Field
              label="Name claim"
              bind:value={draft.oidc.nameClaim}
              spellcheck={false}
              hint="What a commit calls them."
            />
          </div>
        {:else}
          <div class="grid gap-4 sm:grid-cols-[2fr_1fr]">
            <Field
              label="URL"
              bind:value={draft.ldap.url}
              placeholder="ldaps://ipa.example.com"
              spellcheck={false}
              autocomplete="off"
              hint="ldaps://, or ldap:// with StartTLS. Never in the clear."
            />
            <label class="flex items-center gap-2 self-center text-[13px] text-ink-mute sm:pt-1">
              <input type="checkbox" bind:checked={draft.ldap.startTls} class="size-3.5 accent-signal" />
              StartTLS on ldap://
            </label>
          </div>
          <div class="grid gap-4 sm:grid-cols-2">
            <Field
              label="Bind DN"
              bind:value={draft.ldap.bindDn}
              placeholder="uid=wegweiser,cn=sysaccounts,cn=etc,dc=example,dc=com"
              spellcheck={false}
              autocomplete="off"
              hint="Who searches for a person's entry. Empty searches anonymously."
            />
            <Field
              label="Bind password"
              type="password"
              bind:value={draft.ldap.bindPassword}
              autocomplete="new-password"
              placeholder={creating ? "" : "kept; type a new one to replace it"}
              hint="Never shown again once saved."
            />
            <Field
              label="User base"
              bind:value={draft.ldap.userBase}
              placeholder="cn=users,cn=accounts,dc=example,dc=com"
              spellcheck={false}
            />
            <Field
              label="User filter"
              bind:value={draft.ldap.userFilter}
              spellcheck={false}
              hint={"{username} is replaced, escaped. Active Directory: (sAMAccountName={username})"}
            />
            <Field
              label="Name attribute"
              bind:value={draft.ldap.nameAttribute}
              spellcheck={false}
              hint="What a commit calls them."
            />
          </div>

          <fieldset class="flex flex-col gap-2">
            <legend class="sign mb-1 text-[11px] text-ink-faint">Groups are read from</legend>
            {#each groupSources as choice (choice.value)}
              <label
                class="flex cursor-pointer items-start gap-2.5 rounded-sm border px-3 py-2
                       transition-colors {draft.ldap.groups === choice.value
                  ? 'border-signal bg-signal-lo'
                  : 'border-line bg-surface hover:bg-raised'}"
              >
                <input
                  type="radio"
                  name="groups"
                  value={choice.value}
                  bind:group={draft.ldap.groups}
                  class="mt-1 accent-[var(--signal)]"
                />
                <span class="flex flex-col gap-0.5">
                  <span class="text-[13px] {draft.ldap.groups === choice.value ? 'text-signal' : ''}">
                    {choice.label}
                  </span>
                  <span class="text-[12px] text-ink-mute">{choice.what}</span>
                </span>
              </label>
            {/each}
          </fieldset>

          {#if draft.ldap.groups === "search"}
            <div class="grid gap-4 sm:grid-cols-2">
              <Field
                label="Group base"
                bind:value={draft.ldap.groupBase}
                placeholder="cn=groups,cn=accounts,dc=example,dc=com"
                spellcheck={false}
              />
              <Field
                label="Group filter"
                bind:value={draft.ldap.groupFilter}
                spellcheck={false}
                hint={"{dn} is their entry. Nested AD groups: (member:1.2.840.113556.1.4.1941:={dn})"}
              />
            </div>
          {/if}
        {/if}
      </Panel>

      <Panel heading="Trust">
        {#snippet blurb()}
          For a provider whose certificate an internal CA signed, as FreeIPA's is. Empty trusts
          what this machine trusts.
        {/snippet}
        <label class="flex flex-col gap-1.5">
          <span class="sign text-[11px] text-ink-faint">CA certificate</span>
          <textarea
            bind:value={draft.caCertificate}
            rows="5"
            spellcheck={false}
            placeholder="-----BEGIN CERTIFICATE-----"
            class={areaClass}
          ></textarea>
        </label>
      </Panel>

      {#if stored}
        <Panel heading="Remove it">
          {#snippet blurb()}
            Every session opened through it ends at once, on every member. Switching it off
            above does the same and keeps the settings.
          {/snippet}
          <div>
            <Button
              type="button"
              onclick={() => ((removing = true), (refused = null))}
              class="border-crit text-crit hover:border-crit hover:bg-crit-lo"
            >
              Remove {stored.name}
            </Button>
          </div>
        </Panel>
      {/if}
    </form>
  {/if}
</div>

<Dialog open={removing} onclose={() => (removing = false)} title="Remove {stored?.name ?? ''}">
  <p class="text-[13px] text-ink-mute">
    Nobody signs in through it any more, and everybody who did is signed out. The commits they
    made keep their names.
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
    <Button weight="quiet" onclick={() => (removing = false)}>Cancel</Button>
    <Button
      weight="primary"
      onclick={remove}
      disabled={saving}
      class="border-crit bg-crit text-ground hover:border-crit hover:bg-crit"
    >
      {saving ? "Removing…" : "Remove it"}
    </Button>
  {/snippet}
</Dialog>
