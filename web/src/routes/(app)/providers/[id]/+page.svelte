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
  import Row from "#lib/components/Row.svelte";
  import Section from "#lib/components/Section.svelte";

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
  /** The fields as they stood when checked was tried. */
  let checkedDraft = $state("");
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
    checking = true;
    refused = null;
    try {
      const username = draft.kind === "ldap" ? lookup.trim() : "";
      const tried = connection(draft);
      checked = await api.post("/identity-providers/check", {
        body: {
          id: stored?.id,
          provider: toInput(draft),
          ...(username ? { username } : {}),
        },
      });
      checkedDraft = tried;
      document.getElementById("check")?.scrollIntoView({ block: "nearest" });
    } catch (err) {
      checked = null;
      refused =
        err instanceof ApiError ? (err.detail ?? err.title) : "The provider could not be checked.";
    } finally {
      checking = false;
    }
  }

  /**
   * connection is the draft less its mappings: what a check tries. Giving a
   * group a scope changes who gets in, which the check's person shows live,
   * and leaves what the check found standing.
   */
  const connection = (d: Draft) => JSON.stringify({ ...d, groups: null });

  /** The connection has changed since the check on screen, so it describes something else. */
  const stale = $derived(checked !== null && checkedDraft !== connection(draft));

  /**
   * shortName is a group by the value of its first RDN, which is what a
   * mapping usually names it by and what the server compares too (D51).
   */
  function shortName(dn: string): string {
    const first = dn.split(",")[0] ?? dn;
    const at = first.indexOf("=");
    return at >= 0 ? first.slice(at + 1) : dn;
  }

  /** mappedTo is the scope a group is given here, by its DN or its short name. */
  function mappedTo(dn: string): Scope | null {
    const names = [dn.toLowerCase(), shortName(dn).toLowerCase()];
    for (const { scope } of scopes) {
      if (draft.groups[scope].some((g) => names.includes(g.toLowerCase()))) return scope;
    }
    return null;
  }

  /** assign gives a group the scope, or takes it away when it already has it. */
  function assign(dn: string, scope: Scope) {
    const had = mappedTo(dn);
    const names = [dn.toLowerCase(), shortName(dn).toLowerCase()];
    for (const { scope: s } of scopes) {
      draft.groups[s] = draft.groups[s].filter((g) => !names.includes(g.toLowerCase()));
    }
    if (had !== scope) draft.groups[scope] = [...draft.groups[scope], shortName(dn)];
  }

  /**
   * shownSteps is what the check tried, less its last step when it found
   * somebody: that step is the scope their groups gave them when the check ran,
   * and the person below says the same thing for the mappings as they stand
   * now, which a click may have changed since.
   */
  const shownSteps = $derived(
    checked ? (checked.person ? checked.steps.slice(0, -1) : checked.steps) : [],
  );

  /** wouldHold is the scope the person looked up gets with the mappings as they stand. */
  const wouldHold = $derived.by(() => {
    const groups = checked?.person?.groups ?? [];
    for (const { scope } of scopes) {
      if (groups.some((g) => mappedTo(g) === scope)) return scope;
    }
    return null;
  });

  /** works is the verdict: nothing the check tried failed, and whoever it found gets in. */
  const works = $derived(
    !shownSteps.some((s) => s.status === "failed") && (!checked?.person || wouldHold !== null),
  );

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
  /** The grid the connection's fields share, as many to a row as the panel holds. */
  const fieldsClass = "grid grid-cols-[repeat(auto-fill,minmax(15rem,1fr))] gap-4";
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
    {#if allowed && !loading && !trouble}
      {#if stored}
        <Button
          onclick={() => ((removing = true), (refused = null))}
          class="text-crit hover:border-crit hover:bg-crit-lo"
        >
          Remove
        </Button>
      {/if}
      <Button weight="primary" type="submit" form="provider" disabled={saving || !dirty}>
        {saving ? "Saving…" : creating ? "Add provider" : "Save"}
      </Button>
    {/if}
  {/snippet}
</Bar>

<div class="min-h-0 flex-1 overflow-auto">
  {#if !allowed}
    <div class="px-5 py-6 sm:px-8">
      <Notice tone="warn" title="This session may not manage providers">
        A provider decides who may sign in and with what, so seeing or changing one needs the
        admin scope.
      </Notice>
    </div>
  {:else if trouble}
    <div class="px-5 py-6 sm:px-8">
      <Notice tone="crit" title="The provider could not be read">
        {trouble}
        {#snippet actions()}
          <Button onclick={() => load(id)}>Try again</Button>
        {/snippet}
      </Notice>
    </div>
  {:else if loading}
    <p class="px-5 py-6 text-[13px] text-ink-faint sm:px-8">Reading the provider…</p>
  {:else}
    <!-- The form on the left, and beside it the check, which stays in view
         while the form scrolls so that a change and what it does are seen
         together. -->
    <div class="grid gap-x-10 gap-y-8 px-5 py-6 sm:px-8 xl:grid-cols-[minmax(0,1fr)_24rem]">
      <form id="provider" class="flex min-w-0 flex-col gap-12" onsubmit={save}>
        {#if refused || said}
          <div class="flex flex-col gap-3">
            {#if refused}
              <Notice tone="crit" title="That did not work">{refused}</Notice>
            {/if}
            {#if said}
              <Notice tone="signal" title="Done">{said}</Notice>
            {/if}
          </div>
        {/if}

        <Section heading={draft.kind === "oidc" ? "The provider" : "The directory"}>
          {#snippet blurb()}
            {#if draft.kind === "oidc"}
              Signing in uses the authorization code flow with PKCE. Register this server with
              the provider as a confidential client, then copy what it says here.
            {:else}
              A person's entry is found with the filter, and their password checked by binding
              as it, over LDAPS or StartTLS. The password is passed on and forgotten.
            {/if}
          {/snippet}

          {#if creating}
            <Row title="Kind">
              {#snippet about()}
                Which protocol this server speaks to it. It stays the kind it is made as.
              {/snippet}
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
            </Row>
          {/if}

          <Row title="Name">
            {#snippet about()}
              What the sign-in page offers it as.
            {/snippet}
            <div class="flex flex-wrap items-end gap-x-6 gap-y-3">
              <div class="min-w-56 flex-1">
                <Field
                  label="Name"
                  bind:value={draft.name}
                  placeholder={draft.kind === "oidc" ? "Authentik" : "FreeIPA"}
                  autocomplete="off"
                />
              </div>
              <label class="flex h-9 items-center gap-2 text-[13px] text-ink-mute">
                <input type="checkbox" bind:checked={draft.enabled} class="size-3.5 accent-signal" />
                Offered on the sign-in page
              </label>
            </div>
          </Row>

          {#if draft.kind === "oidc"}
            <Row title="Issuer">
              {#snippet about()}
                The discovery document is read from below it.
              {/snippet}
              <Field
                label="Issuer"
                bind:value={draft.oidc.issuer}
                placeholder="https://auth.example.com/application/o/wegweiser/"
                spellcheck={false}
                autocomplete="off"
              />
            </Row>

            <Row title="Client">
              {#snippet about()}
                What the provider registered this server as. The secret is never shown again once
                saved.
              {/snippet}
              <div class={fieldsClass}>
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
                />
              </div>
            </Row>

            <Row title="Redirect URIs">
              {#snippet about()}
                Where the provider sends a person back. One for each address a member is reached
                by, each registered with the provider too.
              {/snippet}
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
            </Row>

            <Row title="Claims">
              {#snippet about()}
                What is asked for at sign-in, where a person's groups are read from, and what a
                commit calls them.
              {/snippet}
              <div class={fieldsClass}>
                <Field label="Scopes" bind:value={draft.oidc.scopes} spellcheck={false} />
                <Field label="Groups claim" bind:value={draft.oidc.groupsClaim} spellcheck={false} />
                <Field label="Name claim" bind:value={draft.oidc.nameClaim} spellcheck={false} />
              </div>
            </Row>
          {:else}
            <Row title="Address">
              {#snippet about()}
                ldaps://, or ldap:// with StartTLS. A password never crosses the network in the
                clear.
              {/snippet}
              <div class="flex flex-wrap items-end gap-x-6 gap-y-3">
                <div class="min-w-56 flex-1">
                  <Field
                    label="URL"
                    bind:value={draft.ldap.url}
                    placeholder="ldaps://ipa.example.com"
                    spellcheck={false}
                    autocomplete="off"
                  />
                </div>
                <label class="flex h-9 items-center gap-2 text-[13px] text-ink-mute">
                  <input type="checkbox" bind:checked={draft.ldap.startTls} class="size-3.5 accent-signal" />
                  StartTLS on ldap://
                </label>
              </div>
            </Row>

            <Row title="Searching as">
              {#snippet about()}
                Who searches for a person's entry. Left empty, the search is anonymous. The
                password is never shown again once saved.
              {/snippet}
              <div class={fieldsClass}>
                <Field
                  label="Bind DN"
                  bind:value={draft.ldap.bindDn}
                  placeholder="uid=wegweiser,cn=sysaccounts,cn=etc,dc=example,dc=com"
                  spellcheck={false}
                  autocomplete="off"
                />
                <Field
                  label="Bind password"
                  type="password"
                  bind:value={draft.ldap.bindPassword}
                  autocomplete="new-password"
                  placeholder={creating ? "" : "kept; type a new one to replace it"}
                />
              </div>
            </Row>

            <Row title="Finding a person">
              {#snippet about()}
                Where people are, the filter that finds one, with {"{username}"} replaced and
                escaped, and the attribute a commit calls them by.
              {/snippet}
              <div class={fieldsClass}>
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
                  hint={"Active Directory: (sAMAccountName={username})"}
                />
                <Field label="Name attribute" bind:value={draft.ldap.nameAttribute} spellcheck={false} />
              </div>
            </Row>

            <Row title="Their groups">
              {#snippet about()}
                Where the directory says which groups a person is in.
              {/snippet}
              <fieldset class="grid gap-2 sm:grid-cols-2">
                <legend class="sr-only">Groups are read from</legend>
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
                <div class={fieldsClass}>
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
            </Row>
          {/if}
        </Section>

        <Section heading="Who it lets in">
          {#snippet blurb()}
            A person holds the strongest scope any of their groups is given here. A person in none
            of these groups is refused, however well they signed in.
            {#if draft.kind === "oidc"}
              A group is a value of the {draft.oidc.groupsClaim || "groups"} claim, as the provider
              sends it.
            {:else}
              A group is its DN, or the value of its first RDN. Looking somebody up in the check
              lists theirs, to give a scope with one click.
            {/if}
          {/snippet}

          {#if nobody}
            <div class="pt-4">
              <Notice tone="warn" title="Nobody can sign in through it yet">
                Give at least one group a scope. Until then every sign-in through this provider is
                refused.
              </Notice>
            </div>
          {/if}

          {#each scopes as { scope, what } (scope)}
            <Row title={scope}>
              {#snippet about()}
                {what}
              {/snippet}
              <ListEditor
                label="Groups given {scope}"
                bind:items={draft.groups[scope]}
                placeholder={draft.kind === "oidc" ? `dns-${scope}` : `dns-${scope}, or its DN`}
                empty="No group is given {scope}."
              />
            </Row>
          {/each}
        </Section>

        <Section heading="Trust">
          <Row title="CA certificate">
            {#snippet about()}
              For a provider whose certificate an internal CA signed, as FreeIPA's is. Empty
              trusts what this machine trusts.
            {/snippet}
            <textarea
              bind:value={draft.caCertificate}
              aria-label="CA certificate"
              rows="6"
              spellcheck={false}
              placeholder="-----BEGIN CERTIFICATE-----"
              class={areaClass}
            ></textarea>
          </Row>
        </Section>
      </form>

      <aside id="check" class="flex flex-col gap-4 xl:sticky xl:top-6 xl:self-start" aria-label="Check it">
        <div class="flex items-center justify-between gap-3 border-b border-line pb-2.5">
          <h2 class="font-cond text-[15px] font-bold tracking-[0.08em] uppercase">Check it</h2>
          <Button onclick={check} disabled={checking}>{checking ? "Checking…" : "Check"}</Button>
        </div>
        <p class="text-[12px] text-ink-mute">
          Tries the fields as they stand, saved or not, from this member and short of somebody
          signing in. Nothing is saved by it.
        </p>

        {#if draft.kind === "ldap"}
          <Field
            label="Look somebody up"
            bind:value={lookup}
            placeholder="alice"
            autocomplete="off"
            spellcheck={false}
            hint="Optional. Who they are and which groups they are in, without a password."
            onkeydown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                void check();
              }
            }}
          />
        {/if}

        {#if checked}
          <p class="text-[13px] {works ? 'text-ok' : 'text-crit'}" role="status">
            {works ? "It works as far as can be told from here." : "It does not work as it stands."}
          </p>
          {#if stale}
            <p class="text-xs text-warn">The fields have changed since. Check again to try them.</p>
          {/if}
          <ol class="flex flex-col gap-2.5" aria-label="What was tried">
            {#each shownSteps as step, i (i)}
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
            {@const person = checked.person}
            <div class="flex flex-col gap-3 rounded-sm border border-line bg-surface p-3">
              <div class="flex flex-col gap-0.5">
                <p class="flex flex-wrap items-center gap-2 text-[13px]">
                  <span class="text-ink">{person.name}</span>
                  {#if wouldHold}
                    <Chip tone="signal">{wouldHold}</Chip>
                  {:else}
                    <Chip tone="crit">refused</Chip>
                  {/if}
                </p>
                <p class="num text-[11px] break-all text-ink-faint">{person.dn}</p>
              </div>
              {#if person.groups.length > 0}
                <ul class="flex flex-col divide-y divide-line-soft" aria-label="Their groups">
                  {#each person.groups as group (group)}
                    <li class="flex items-center justify-between gap-2 py-1.5">
                      <span class="num min-w-0 truncate text-[12px] text-ink" title={group}>
                        {shortName(group)}
                      </span>
                      <span class="flex shrink-0 gap-1" role="group" aria-label="Scope for {shortName(group)}">
                        {#each scopes as { scope } (scope)}
                          <button
                            type="button"
                            aria-pressed={mappedTo(group) === scope}
                            onclick={() => assign(group, scope)}
                            class="sign h-6 cursor-pointer rounded-xs border px-1.5 text-[10px] transition-colors
                                   {mappedTo(group) === scope
                              ? 'border-signal bg-signal-lo text-signal'
                              : 'border-line text-ink-faint hover:border-ink-faint hover:text-ink'}"
                          >
                            {scope}
                          </button>
                        {/each}
                      </span>
                    </li>
                  {/each}
                </ul>
                <p class="text-[11px] text-ink-faint">
                  A click gives the group that scope here, and another takes it away. Save to keep it.
                </p>
              {:else}
                <p class="text-[12px] text-ink-faint">In no group, so a sign-in is refused.</p>
              {/if}
            </div>
          {/if}
        {:else}
          <p class="text-xs text-ink-faint">Nothing tried yet.</p>
        {/if}
      </aside>
    </div>
  {/if}
</div>

<Dialog open={removing} onclose={() => (removing = false)} title="Remove {stored?.name ?? ''}">
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
