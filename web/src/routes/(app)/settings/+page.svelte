<script lang="ts">
  /**
   * The server's own settings: what a zone that says nothing about itself
   * inherits. They live in the database rather than in the configuration file,
   * so this screen and `weg settings` reach the same thing and a change takes
   * effect on the next write (docs/decisions/ D11).
   *
   * Writing the file the other end of a transfer needs is not one of these. It
   * is a tool rather than a setting, and it lives under Secondaries, where
   * `weg secondary config` puts it too.
   */
  import { api, ApiError } from "$lib/api";
  import type { ReverseConflictPolicy, Settings } from "$lib/api";
  import { session } from "$lib/session.svelte";
  import Bar from "$lib/components/Bar.svelte";
  import ListSetting from "$lib/components/ListSetting.svelte";
  import Notice from "$lib/components/Notice.svelte";
  import Panel from "$lib/components/Panel.svelte";

  let settings = $state<Settings | null>(null);
  let failed = $state<string | null>(null);
  let refused = $state<string | null>(null);
  let saving = $state<ReverseConflictPolicy | null>(null);
  let allow = $state("");
  let savingAllow = $state(false);
  let notify = $state("");
  let savingNotify = $state(false);

  const writable = $derived(session.can("write"));

  $effect(() => {
    void load();
  });

  async function load() {
    try {
      settings = await api.get("/settings");
      allow = settings.transferAllow.join(", ");
      notify = settings.notifyTargets.join(", ");
      failed = null;
    } catch (err) {
      failed = err instanceof ApiError ? (err.detail ?? err.title) : "The settings could not be read.";
    }
  }

  /**
   * Choosing saves at once. There is one setting and no draft to lose, so a
   * Save button would be a second click that only delays the same outcome.
   */
  async function choose(policy: ReverseConflictPolicy) {
    if (!writable || policy === settings?.reverseConflictPolicy) return;
    saving = policy;
    refused = null;
    try {
      settings = await api.patch("/settings", { body: { reverseConflictPolicy: policy } });
    } catch (err) {
      refused = err instanceof ApiError ? (err.detail ?? err.title) : "The change was not saved.";
    } finally {
      saving = null;
    }
  }

  /**
   * Unlike the policy above these have a draft to lose, so they save on a
   * button rather than on every keystroke.
   */
  async function saveAllow() {
    if (!writable) return;
    savingAllow = true;
    refused = null;
    try {
      settings = await api.patch("/settings", { body: { transferAllow: split(allow) } });
      allow = settings.transferAllow.join(", ");
    } catch (err) {
      refused = err instanceof ApiError ? (err.detail ?? err.title) : "The change was not saved.";
    } finally {
      savingAllow = false;
    }
  }

  async function saveNotify() {
    if (!writable) return;
    savingNotify = true;
    refused = null;
    try {
      settings = await api.patch("/settings", { body: { notifyTargets: split(notify) } });
      notify = settings.notifyTargets.join(", ");
    } catch (err) {
      refused = err instanceof ApiError ? (err.detail ?? err.title) : "The change was not saved.";
    } finally {
      savingNotify = false;
    }
  }

  /**
   * split takes a list the way somebody types one. Commas only: an entry may
   * hold a space, because a notify target names its key after the address.
   */
  const split = (v: string) =>
    v
      .split(",")
      .map((entry) => entry.trim())
      .filter(Boolean);

  /**
   * The wording follows docs/decisions/ D3. Each says what the server does,
   * not what the value is called, because the name alone does not tell you
   * which of two names ends up answering.
   */
  const policies: { value: ReverseConflictPolicy; label: string; what: string }[] = [
    {
      value: "first-wins",
      label: "Keep the first",
      what:
        "The name already answering keeps the address, and the write reports the conflict. " +
        "The only setting that never changes an answer nobody asked to change.",
    },
    {
      value: "last-wins",
      label: "Take it over",
      what:
        "The new name takes the address, and the conflict is still reported. A reverse " +
        "entry somebody wrote by hand is never replaced.",
    },
    {
      value: "multi",
      label: "Keep both",
      what:
        "Every name answers. The literal reading of “generate the reverse entry”, and " +
        "it turns a routine change into a multi-record PTR set that reverse-lookup checks " +
        "in mail and logging are not built for.",
    },
    {
      value: "reject",
      label: "Refuse the write",
      what: "The whole change fails, address record included.",
    },
  ];
</script>

<svelte:head><title>Settings — Wegweiser</title></svelte:head>

<Bar title="Settings" />

<div class="flex flex-1 flex-col overflow-auto px-5 py-6">
  <div class="mx-auto flex w-full max-w-3xl flex-col gap-7">
    <div class="flex flex-col gap-3 empty:hidden">
      {#if failed}
        <Notice tone="crit" title="The settings could not be read">{failed}</Notice>
      {/if}
      {#if refused}
        <Notice tone="crit" title="The change was not saved">{refused}</Notice>
      {/if}
      {#if !writable}
        <Notice tone="signal" title="Read only">
          Changing a server-wide setting needs a token with the write scope.
        </Notice>
      {/if}
    </div>

    <div class="flex flex-col gap-3">
      <div class="flex items-center gap-3">
        <h2 class="sign text-[11px] text-ink-mute">Reverse entries</h2>
        <span class="h-px flex-1 bg-line"></span>
      </div>

      <Panel heading="When an address already answers">
        {#snippet blurb()}
          Several names on one address is the normal case: a virtual host, a load balancer, a
          service alias. This is what happens to the reverse entry when the second one
          arrives. A zone cannot override it; it is the whole server.
        {/snippet}

        {#if settings === null && failed === null}
          <p class="text-[12px] text-ink-mute">Reading the settings…</p>
        {:else if settings}
          <div class="flex flex-col gap-px overflow-hidden rounded-sm bg-line">
            {#each policies as p (p.value)}
              <button
                type="button"
                disabled={!writable || saving !== null}
                onclick={() => choose(p.value)}
                aria-pressed={settings.reverseConflictPolicy === p.value}
                class="group flex cursor-pointer flex-col gap-1 bg-surface px-4 py-3 text-left
                       transition-colors hover:bg-raised disabled:cursor-not-allowed
                       aria-pressed:bg-signal-lo"
              >
                <span class="flex items-center gap-2">
                  <span
                    class="size-1.5 shrink-0 rounded-full bg-ink-faint
                           group-aria-pressed:bg-signal"
                  ></span>
                  <span class="sign text-[11px] text-ink group-aria-pressed:text-signal">
                    {p.label}
                  </span>
                  <span class="num text-[10px] text-ink-faint">{p.value}</span>
                  {#if saving === p.value}
                    <span class="text-[10px] text-ink-mute">saving…</span>
                  {/if}
                </span>
                <span class="pl-3.5 text-[12px] text-ink-mute">{p.what}</span>
              </button>
            {/each}
          </div>
        {/if}
      </Panel>
    </div>

    <div class="flex flex-col gap-3">
      <div class="flex items-center gap-3">
        <h2 class="sign text-[11px] text-ink-mute">Zone transfer</h2>
        <span class="h-px flex-1 bg-line"></span>
      </div>

      <Panel heading="Who may pull a whole zone">
        {#snippet blurb()}
          A zone transfer hands over every name and every address at once, which is an
          inventory of the network. Nobody may until somebody is named here, and the same list
          decides who may ask for only what has changed since their copy. Addresses or CIDR
          prefixes, separated by commas; a bare address means that host alone.
        {/snippet}

        {#if settings}
          <ListSetting
            label="Allowed clients"
            placeholder="192.0.2.0/24, 2001:db8::1"
            bind:value={allow}
            stored={settings.transferAllow}
            reads="Transfers go to"
            empty="nobody"
            disabled={!writable}
            saving={savingAllow}
            onsave={saveAllow}
          />
        {/if}
      </Panel>

      <Panel heading="Who is told when a zone changes">
        {#snippet blurb()}
          A secondary that is not told finds out when its own refresh timer fires, which is
          usually an hour. This is a second list on purpose: the one above decides who may
          take a copy, this one says where the news can arrive, so it holds addresses rather
          than networks. A port may follow a colon, and a key after the address signs the
          notification for a secondary that insists on one.
        {/snippet}

        {#if settings}
          <ListSetting
            label="Secondaries"
            placeholder="192.0.2.53, 198.51.100.53 key:secondary.example.com."
            bind:value={notify}
            stored={settings.notifyTargets}
            reads="A change is announced to"
            empty="nobody"
            disabled={!writable}
            saving={savingNotify}
            onsave={saveNotify}
          />
        {/if}
      </Panel>
    </div>
  </div>
</div>
