<script lang="ts">
  /**
   * The zone's own settings: the start of authority, and the handful of things
   * about a zone that are not records.
   */
  import { untrack } from "svelte";

  import { invalidateAll } from "$app/navigation";
  import { api, ApiError } from "$lib/api";
  import type { Zone } from "$lib/api";
  import { exact } from "$lib/format";
  import { session } from "$lib/session.svelte";
  import Button from "$lib/components/Button.svelte";
  import Field from "$lib/components/Field.svelte";
  import Notice from "$lib/components/Notice.svelte";

  let { data } = $props();

  /** Draft is the editable part of a zone, as the strings the fields hold. */
  interface Draft {
    primaryNs: string;
    mailbox: string;
    refresh: string;
    retry: string;
    expire: string;
    minimum: string;
    soaTtl: string;
    defaultTtl: string;
    comment: string;
    /** Three states, and null is not false: it means "follow the server". */
    autoReverse: boolean | null;
    disabled: boolean;
  }

  function toDraft(z: Zone): Draft {
    return {
      primaryNs: z.soa.primaryNs,
      mailbox: z.soa.mailbox,
      refresh: String(z.soa.refresh),
      retry: String(z.soa.retry),
      expire: String(z.soa.expire),
      minimum: String(z.soa.minimum),
      soaTtl: String(z.soa.ttl),
      defaultTtl: String(z.defaultTtl),
      comment: z.comment ?? "",
      autoReverse: z.autoReverse ?? null,
      disabled: z.disabled,
    };
  }

  const zone = $derived(data.zone);

  // Untracked on purpose: the effect below owns keeping this in step with the
  // zone, and a tracked read here would make the initialiser look like the
  // thing that does it.
  let draft = $state<Draft>(untrack(() => toDraft(data.zone)));
  let saving = $state(false);
  let refused = $state<string | null>(null);
  let saved = $state(false);

  // The zone can change under us: a save reloads it, and so does navigating
  // between zones without leaving this page.
  $effect(() => {
    draft = toDraft(data.zone);
  });

  const dirty = $derived(JSON.stringify(draft) !== JSON.stringify(toDraft(zone)));
  const writable = $derived(session.can("write"));

  /** seconds turns a field back into what the API wants, refusing nonsense. */
  function seconds(raw: string, what: string): number {
    const value = Number(raw.trim());
    if (!Number.isInteger(value) || value < 0) {
      throw new Error(`${what} has to be a whole number of seconds`);
    }
    return value;
  }

  async function save() {
    saving = true;
    refused = null;
    saved = false;
    try {
      await api.patch("/zones/{zoneId}", {
        path: { zoneId: zone.id },
        body: {
          soa: {
            primaryNs: draft.primaryNs.trim(),
            mailbox: draft.mailbox.trim(),
            refresh: seconds(draft.refresh, "Refresh"),
            retry: seconds(draft.retry, "Retry"),
            expire: seconds(draft.expire, "Expire"),
            minimum: seconds(draft.minimum, "Minimum"),
            ttl: seconds(draft.soaTtl, "The SOA's own TTL"),
          },
          defaultTtl: seconds(draft.defaultTtl, "The default TTL"),
          // Null puts the zone back on the server-wide setting, which is a
          // third state and not the same as false.
          autoReverse: draft.autoReverse,
          disabled: draft.disabled,
          comment: draft.comment.trim(),
        },
      });
      await invalidateAll();
      saved = true;
    } catch (err) {
      refused =
        err instanceof ApiError
          ? (err.detail ?? err.title)
          : err instanceof Error
            ? err.message
            : "The change was not saved.";
    } finally {
      saving = false;
    }
  }

  const reverseChoices = [
    { value: null, label: "Server default" },
    { value: true, label: "On" },
    { value: false, label: "Off" },
  ] as const;
</script>

<div class="flex flex-1 flex-col gap-8 overflow-auto px-5 py-6">
  {#if zone.disabled}
    <Notice tone="warn" title="This zone is not on the wire">
      A disabled zone is invisible rather than merely marked: queries for it are answered as
      though this server held nothing. Its records are untouched and it comes back the moment
      this is switched off.
    </Notice>
  {/if}

  {#if refused}
    <Notice tone="crit" title="The change was not saved">{refused}</Notice>
  {/if}

  <section class="flex flex-col gap-4">
    <div class="flex items-baseline gap-3">
      <h2 class="sign text-[11px] text-ink-faint">Start of authority</h2>
      <p class="text-[12px] text-ink-mute">
        The serial is not here: one commit advances it by exactly one, which is what lets the
        history be replayed.
      </p>
    </div>

    <div class="grid max-w-4xl gap-4 sm:grid-cols-2 lg:grid-cols-3">
      <Field
        label="Primary name server"
        bind:value={draft.primaryNs}
        spellcheck={false}
        disabled={!writable}
        hint="The MNAME field."
      />
      <Field
        label="Mailbox"
        bind:value={draft.mailbox}
        spellcheck={false}
        disabled={!writable}
        hint="RNAME, as a domain name: hostmaster.example.com."
      />
      <Field
        label="SOA record TTL"
        bind:value={draft.soaTtl}
        inputmode="numeric"
        disabled={!writable}
        hint="How long the SOA record itself may be held."
      />
      <Field
        label="Refresh"
        bind:value={draft.refresh}
        inputmode="numeric"
        disabled={!writable}
        hint="How often a secondary checks for a new serial."
      />
      <Field
        label="Retry"
        bind:value={draft.retry}
        inputmode="numeric"
        disabled={!writable}
        hint="How soon it tries again after a failed check."
      />
      <Field
        label="Expire"
        bind:value={draft.expire}
        inputmode="numeric"
        disabled={!writable}
        hint="When a secondary stops answering for a zone it cannot refresh."
      />
      <Field
        label="Minimum"
        bind:value={draft.minimum}
        inputmode="numeric"
        disabled={!writable}
        hint="The negative-caching TTL of RFC 2308 §4, not a floor on record TTLs."
      />
    </div>
  </section>

  <section class="flex flex-col gap-4">
    <h2 class="sign text-[11px] text-ink-faint">Zone</h2>

    <div class="grid max-w-4xl gap-4 sm:grid-cols-2 lg:grid-cols-3">
      <Field
        label="Default TTL"
        bind:value={draft.defaultTtl}
        inputmode="numeric"
        disabled={!writable}
        hint="Applied to a record added without one."
      />
      <Field
        label="Comment"
        bind:value={draft.comment}
        disabled={!writable}
        hint="For whoever reads this next."
      />

      <div class="flex flex-col gap-1.5">
        <span class="sign text-[11px] text-ink-faint">Automatic reverse</span>
        <div class="flex h-9 items-center rounded-sm border border-line bg-surface p-0.5">
          {#each reverseChoices as choice (choice.label)}
            <button
              type="button"
              disabled={!writable}
              onclick={() => (draft.autoReverse = choice.value)}
              aria-pressed={draft.autoReverse === choice.value}
              class="sign h-full flex-1 cursor-pointer rounded-xs text-[11px] transition-colors
                     disabled:cursor-not-allowed aria-pressed:bg-signal-lo
                     aria-pressed:text-signal not-aria-pressed:text-ink-mute
                     not-aria-pressed:hover:text-ink"
            >
              {choice.label}
            </button>
          {/each}
        </div>
        <p class="text-xs text-ink-mute">
          Whether an address record here writes the matching PTR. Following the server is a
          third state, not the same as off.
        </p>
      </div>

      <div class="flex flex-col gap-1.5">
        <span class="sign text-[11px] text-ink-faint">On the wire</span>
        <div class="flex h-9 items-center rounded-sm border border-line bg-surface p-0.5">
          <button
            type="button"
            disabled={!writable}
            onclick={() => (draft.disabled = false)}
            aria-pressed={!draft.disabled}
            class="sign h-full flex-1 cursor-pointer rounded-xs text-[11px] transition-colors
                   disabled:cursor-not-allowed aria-pressed:bg-ok-lo aria-pressed:text-ok
                   not-aria-pressed:text-ink-mute not-aria-pressed:hover:text-ink"
          >
            Serving
          </button>
          <button
            type="button"
            disabled={!writable}
            onclick={() => (draft.disabled = true)}
            aria-pressed={draft.disabled}
            class="sign h-full flex-1 cursor-pointer rounded-xs text-[11px] transition-colors
                   disabled:cursor-not-allowed aria-pressed:bg-warn-lo aria-pressed:text-warn
                   not-aria-pressed:text-ink-mute not-aria-pressed:hover:text-ink"
          >
            Disabled
          </button>
        </div>
        <p class="text-xs text-ink-mute">
          A disabled zone is answered as though this server held nothing for it.
        </p>
      </div>
    </div>
  </section>

  {#if writable}
    <div class="flex items-center gap-2 border-t border-line pt-5">
      <Button weight="primary" onclick={save} disabled={!dirty || saving}>
        {saving ? "Saving…" : "Save changes"}
      </Button>
      <Button weight="quiet" onclick={() => (draft = toDraft(zone))} disabled={!dirty || saving}>
        Revert
      </Button>
      {#if saved && !dirty}
        <span class="sign text-[11px] text-ok">Saved</span>
      {/if}
    </div>
  {/if}

  <p class="num text-xs text-ink-faint">
    created {exact(zone.createdAt)} · id {zone.id}
  </p>
</div>
