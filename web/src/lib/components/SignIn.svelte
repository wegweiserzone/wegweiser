<script lang="ts">
  /**
   * The session screen.
   */
  import { api, NetworkError } from "$lib/api";
  import type { Health } from "$lib/api";
  import { session } from "$lib/session.svelte";
  import Button from "./Button.svelte";
  import Chip from "./Chip.svelte";
  import Field from "./Field.svelte";
  import Mark from "./Mark.svelte";

  let token = $state("");
  let health = $state<Health | null>(null);
  let reachable = $state(true);

  $effect(() => {
    api
      .get("/healthz")
      .then((h) => {
        health = h;
        reachable = true;
      })
      .catch((err) => {
        // A server that is up but not serving answers 503 with a problem
        // document, which is a different thing from one that is not there.
        reachable = !(err instanceof NetworkError);
        health = null;
      });
  });

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (await session.open(token)) token = "";
  }
</script>

<main class="flex min-h-screen items-center justify-center px-6 py-12">
  <div class="flex w-full max-w-sm flex-col gap-7">
    <div class="flex items-center gap-3">
      <Mark class="size-7 text-signal" />
      <div>
        <p class="font-cond text-2xl leading-none font-bold tracking-[0.13em] uppercase">
          Wegweiser
        </p>
        <p class="num mt-1 text-[11px] text-ink-faint">
          {#if health}
            {health.version} · {health.zones}
            {health.zones === 1 ? "zone" : "zones"} · {health.records.toLocaleString("en")}
            records
          {:else if reachable}
            reachable, not yet serving
          {:else}
            no answer from the server
          {/if}
        </p>
      </div>
      {#if health}
        <Chip tone="ok" dot>Serving</Chip>
      {:else if reachable}
        <Chip tone="warn" dot>Starting</Chip>
      {:else}
        <Chip tone="crit" dot>Down</Chip>
      {/if}
    </div>

    <form class="flex flex-col gap-4" onsubmit={submit}>
      <Field
        label="API token"
        type="password"
        autocomplete="current-password"
        spellcheck={false}
        placeholder="weg_…"
        bind:value={token}
        hint="Exchanged once for a session cookie a script on this page cannot read. The token itself is never stored in the browser."
      />

      {#if session.refused}
        <p
          class="rounded-sm border border-line border-l-2 border-l-crit bg-surface px-3 py-2
                 text-[13px] text-ink-mute"
          role="alert"
        >
          {session.refused}
        </p>
      {/if}

      <Button
        weight="primary"
        type="submit"
        disabled={session.busy || token.trim() === ""}
        class="h-9 w-full justify-center"
      >
        {session.busy ? "Opening…" : "Sign in"}
      </Button>
    </form>

    <p class="border-t border-line pt-5 text-xs text-ink-mute">
      No token yet? On the server, <code class="num text-ink">weg token create</code> prints
      one. It is shown once, when it is created, and never again.
    </p>
  </div>
</main>
