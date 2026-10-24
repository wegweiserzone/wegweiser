<script lang="ts">
  /**
   * The session screen.
   */
  import { onMount } from "svelte";
  import { replaceState } from "$app/navigation";
  import { api, NetworkError, oidcStart, providers } from "#lib/api/index.ts";
  import type { Health, SignInProvider } from "#lib/api/index.ts";
  import { session } from "#lib/session.svelte.ts";
  import Button from "./Button.svelte";
  import Chip from "./Chip.svelte";
  import Field from "./Field.svelte";
  import Mark from "./Mark.svelte";

  let token = $state("");
  let health = $state<Health | null>(null);
  let reachable = $state(true);

  // What may be signed in through beside a token (D51). A token always works,
  // so a server that cannot say leaves the token form alone on the page.
  let offered = $state<SignInProvider[]>([]);
  const redirected = $derived(offered.filter((p) => p.kind === "oidc"));
  const directories = $derived(offered.filter((p) => p.kind === "ldap"));

  /** method is "token", or the directory being signed in through. */
  let method = $state("token");
  let username = $state("");
  let password = $state("");
  const directory = $derived(directories.find((d) => d.id === method));

  onMount(() => {
    providers()
      .then((p) => (offered = p))
      .catch(() => (offered = []));

    // A provider that refused sends the browser back here with the reason.
    // It is shown once and taken out of the address, so that a reload does
    // not say it again.
    const url = new URL(window.location.href);
    const reason = url.searchParams.get("signin_error");
    if (reason) {
      session.refuse(reason);
      url.searchParams.delete("signin_error");
      replaceState(url, {});
    }
  });

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
    if (directory) {
      const opened = await session.openThrough(directory.id, username, password);
      password = "";
      if (opened) username = "";
      return;
    }
    if (await session.open(token)) token = "";
  }

  const ready = $derived(
    directory ? username.trim() !== "" && password !== "" : token.trim() !== "",
  );
</script>

<main class="flex min-h-screen items-center justify-center px-6 py-12">
  <div class="flex w-full max-w-sm flex-col gap-7">
    <!-- The status sits at the right edge, lined up with the field below. Left
         to follow the text it trailed the wider of the two lines, which put it
         beside the title looking like it had landed there by accident. -->
    <div class="flex items-center justify-between gap-3">
      <div class="flex min-w-0 items-center gap-3">
        <Mark class="size-7 shrink-0 text-signal" />
        <div class="min-w-0">
          <p class="font-cond text-2xl leading-none font-bold tracking-[0.13em] uppercase">
            Wegweiser
          </p>
          <p class="num mt-1 truncate text-[11px] text-ink-faint">
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
      </div>
      {#if health}
        <Chip tone="ok" dot>Serving</Chip>
      {:else if reachable}
        <Chip tone="warn" dot>Starting</Chip>
      {:else}
        <Chip tone="crit" dot>Down</Chip>
      {/if}
    </div>

    {#if redirected.length > 0}
      <div class="flex flex-col gap-2">
        {#each redirected as p (p.id)}
          <Button
            type="button"
            class="h-9 w-full justify-center"
            onclick={() => window.location.assign(oidcStart(p.id))}
          >
            Sign in with {p.name}
          </Button>
        {/each}
      </div>
    {/if}

    <form class="flex flex-col gap-4" onsubmit={submit}>
      {#if directories.length > 0}
        <div class="flex flex-wrap gap-1.5" role="group" aria-label="Sign in with">
          {#each [{ id: "token", name: "API token" }, ...directories] as m (m.id)}
            <Button
              type="button"
              aria-pressed={method === m.id}
              weight={method === m.id ? "primary" : "default"}
              class="h-7"
              onclick={() => (method = m.id)}
            >
              {m.name}
            </Button>
          {/each}
        </div>
      {/if}

      {#if directory}
        <Field label="Username" autocomplete="username" spellcheck={false} bind:value={username} />
        <Field
          label="Password"
          type="password"
          autocomplete="current-password"
          bind:value={password}
          hint="Passed to {directory.name} to check, and kept nowhere."
        />
      {:else}
        <Field
          label="API token"
          type="password"
          autocomplete="current-password"
          spellcheck={false}
          placeholder="weg_…"
          bind:value={token}
          hint="Exchanged once for a session cookie a script on this page cannot read. The token itself is never stored in the browser."
        />
      {/if}

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
        disabled={session.busy || !ready}
        class="h-9 w-full justify-center"
      >
        {session.busy ? "Opening…" : "Sign in"}
      </Button>
    </form>

    {#if !directory}
      <p class="border-t border-line pt-5 text-xs text-ink-mute">
        No token yet? On the server, <code class="num text-ink">weg token create</code> prints
        one. It is shown once, when it is created, and never again.
      </p>
    {/if}
  </div>
</main>
