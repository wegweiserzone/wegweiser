<script lang="ts">
  /**
   * The server's own settings: what a zone that says nothing about itself
   * inherits. They live in the database rather than in the configuration file,
   * so this screen and `weg settings` reach the same thing and a change takes
   * effect on the next write (docs/decisions.md D11).
   */
  import { api, ApiError } from "$lib/api";
  import type { ReverseConflictPolicy, Settings } from "$lib/api";
  import { session } from "$lib/session.svelte";
  import Notice from "$lib/components/Notice.svelte";

  let settings = $state<Settings | null>(null);
  let failed = $state<string | null>(null);
  let refused = $state<string | null>(null);
  let saving = $state<ReverseConflictPolicy | null>(null);

  const writable = $derived(session.can("write"));

  $effect(() => {
    void load();
  });

  async function load() {
    try {
      settings = await api.get("/settings");
      failed = null;
    } catch (err) {
      failed =
        err instanceof ApiError
          ? (err.detail ?? err.title)
          : "The settings could not be read.";
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
      refused =
        err instanceof ApiError
          ? (err.detail ?? err.title)
          : "The change was not saved.";
    } finally {
      saving = null;
    }
  }

  /**
   * The wording follows docs/decisions.md D3. Each says what the server does,
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

<div class="flex flex-1 flex-col gap-8 overflow-auto px-5 py-6">
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

  <section class="flex max-w-3xl flex-col gap-4">
    <div class="flex flex-col gap-1">
      <h2 class="sign text-[11px] text-ink-faint">When an address already answers</h2>
      <p class="text-[12px] text-ink-mute">
        Several names on one address is the normal case: a virtual host, a load balancer, a
        service alias. This is what happens to the reverse entry when the second one arrives.
        A zone cannot override it; it is the whole server.
      </p>
    </div>

    {#if settings === null && failed === null}
      <p class="text-[12px] text-ink-mute">Reading the settings…</p>
    {:else if settings}
      <div class="flex flex-col gap-px overflow-hidden rounded-sm border border-line">
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
  </section>
</div>
