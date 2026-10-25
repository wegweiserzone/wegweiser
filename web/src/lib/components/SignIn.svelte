<script lang="ts">
  /**
   * The session screen.
   *
   * Two halves. The left says which server this is and how it stands, which is
   * all it may say before anybody has signed in. The right is the way in, in
   * the order people should take it: the identity providers first, each a tile
   * saying what it will ask for, and the API token last, folded away, because
   * a token is an operator's tool and a person has an account
   * (docs/decisions/d51-accounts-come-from-an-identity-provider.md). A server
   * with no providers shows the token at once, since it is then the only way.
   */
  import { onMount } from "svelte";
  import { slide } from "svelte/transition";
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
  // so a server that cannot say leaves the token alone on the page.
  let offered = $state<SignInProvider[]>([]);
  let asked = $state(false);

  /** open is the way in being filled in: "token", a directory's id, or nothing. */
  let open = $state<string | null>(null);
  let username = $state("");
  let password = $state("");
  const directory = $derived(offered.find((p) => p.kind === "ldap" && p.id === open));
  /** With nothing else to offer, the token is the way in and is not folded away. */
  const tokenOnly = $derived(asked && offered.length === 0);
  const showToken = $derived(tokenOnly || open === "token");

  // A viewer who asked for less motion gets none: app.css cuts every
  // animation short, these included.
  const fold = { duration: 160 };

  onMount(() => {
    providers()
      .then((p) => (offered = p))
      .catch(() => (offered = []))
      .finally(() => (asked = true));

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

  function toggle(which: string) {
    open = open === which ? null : which;
    password = "";
  }

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
  const host = typeof window !== "undefined" ? window.location.host : "";

  // Paths for the tiles' icons: a door left by for OpenID Connect, which
  // sends the browser elsewhere and back, and a stack of entries for a
  // directory, which is asked here.
  const icons = {
    oidc: "M14 4h5a1 1 0 0 1 1 1v14a1 1 0 0 1-1 1h-5M10 16l4-4-4-4M14 12H4",
    ldap: "M5 5h14v4H5zM5 10h14v4H5zM5 15h14v4H5zM8 7h.01M8 12h.01M8 17h.01",
  };
</script>

<main class="grid min-h-screen lg:grid-cols-[minmax(0,1.15fr)_minmax(0,1fr)]">
  <!-- Which server, and how it stands. -->
  <section
    class="flex flex-col justify-between gap-10 border-line bg-sunken px-6 py-8
           sm:px-10 lg:border-r lg:px-14 lg:py-12"
    aria-label="This server"
  >
    <div class="flex items-center gap-3">
      <Mark class="size-8 shrink-0 text-signal" />
      <p class="font-cond text-[26px] leading-none font-bold tracking-[0.14em] uppercase">
        Wegweiser
      </p>
    </div>

    <div class="flex max-w-xl flex-col gap-3">
      <div>
        {#if health}
          <Chip tone="ok" dot>Serving</Chip>
        {:else if reachable}
          <Chip tone="warn" dot>Starting</Chip>
        {:else}
          <Chip tone="crit" dot>No answer</Chip>
        {/if}
      </div>
      <h1
        class="font-cond text-4xl leading-[1.05] font-bold tracking-[0.04em] break-all uppercase
               sm:text-5xl"
      >
        {host}
      </h1>
      <p class="max-w-md text-[14px] text-ink-mute">
        {#if health}
          An authoritative name server, answering for
          <span class="num text-ink">{health.zones.toLocaleString("en")}</span>
          {health.zones === 1 ? "zone" : "zones"} and
          <span class="num text-ink">{health.records.toLocaleString("en")}</span>
          {health.records === 1 ? "record" : "records"}.
        {:else if reachable}
          The server is up and not yet answering queries. Signing in works meanwhile.
        {:else}
          Nothing answered from this address. The page came from it, so it is shutting down
          or something in between is refusing.
        {/if}
      </p>
    </div>

    <p class="num text-xs text-ink-faint">{health?.version ?? ""}</p>
  </section>

  <!-- The way in. -->
  <section class="flex items-center justify-center px-6 py-10 sm:px-10" aria-label="Sign in">
    <div class="flex w-full max-w-sm flex-col gap-6">
      <div class="flex flex-col gap-1">
        <h2 class="font-cond text-2xl font-bold tracking-[0.08em] uppercase">Sign in</h2>
        <p class="text-[13px] text-ink-mute">
          {#if tokenOnly}
            With an API token. Accounts come from an identity provider once one is set up.
          {:else}
            With the account you already have.
          {/if}
        </p>
      </div>

      {#if session.refused}
        <p
          class="rounded-sm border border-line border-l-2 border-l-crit bg-surface px-3 py-2.5
                 text-[13px] text-ink-mute"
          role="alert"
        >
          {session.refused}
        </p>
      {/if}

      <form class="flex flex-col gap-2.5" onsubmit={submit}>
        {#each offered as p (p.id)}
          {@const expanded = open === p.id}
          <div
            class="overflow-hidden rounded-sm border bg-surface transition-colors
                   {expanded ? 'border-signal' : 'border-line hover:border-ink-faint'}"
          >
            <button
              type="button"
              aria-label="Sign in with {p.name}"
              aria-expanded={p.kind === "ldap" ? expanded : undefined}
              onclick={() => (p.kind === "oidc" ? window.location.assign(oidcStart(p.id)) : toggle(p.id))}
              class="group flex w-full cursor-pointer items-center gap-3.5 px-4 py-3.5 text-left"
            >
              <span
                class="grid size-9 shrink-0 place-items-center rounded-sm transition-colors
                       {expanded ? 'bg-signal-lo text-signal' : 'bg-raised text-ink-mute group-hover:text-ink'}"
              >
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" class="size-4.5" aria-hidden="true">
                  <path d={icons[p.kind]} stroke-linecap="round" stroke-linejoin="round" />
                </svg>
              </span>
              <span class="flex min-w-0 flex-1 flex-col">
                <span class="truncate text-[15px] text-ink">{p.name}</span>
                <span class="text-[12px] text-ink-faint">
                  {p.kind === "oidc" ? "Single sign-on, in your browser" : "Directory: name and password"}
                </span>
              </span>
              <svg
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                stroke-width="2"
                class="size-4 shrink-0 text-ink-faint transition-transform
                       {p.kind === 'ldap' && expanded ? 'rotate-90 text-signal' : ''}"
                aria-hidden="true"
              >
                <path d="m9 6 6 6-6 6" stroke-linecap="round" stroke-linejoin="round" />
              </svg>
            </button>

            {#if expanded && directory}
              <div class="flex flex-col gap-3.5 border-t border-line px-4 py-4" transition:slide={fold}>
                <Field label="Username" autocomplete="username" spellcheck={false} bind:value={username} />
                <Field
                  label="Password"
                  type="password"
                  autocomplete="current-password"
                  bind:value={password}
                  hint="Passed to {directory.name} to check, and kept nowhere."
                />
                <Button
                  weight="primary"
                  type="submit"
                  disabled={session.busy || !ready}
                  class="h-9 w-full justify-center"
                >
                  {session.busy ? "Signing in…" : "Sign in"}
                </Button>
              </div>
            {/if}
          </div>
        {/each}

        {#if !tokenOnly && asked}
          <div class="flex items-center gap-3 pt-2 text-[11px] text-ink-faint">
            <span class="h-px flex-1 bg-line"></span>
            <button
              type="button"
              aria-expanded={showToken}
              onclick={() => toggle("token")}
              class="sign cursor-pointer text-ink-mute transition-colors hover:text-ink"
            >
              {showToken ? "Hide the token" : "Use an API token"}
            </button>
            <span class="h-px flex-1 bg-line"></span>
          </div>
        {/if}

        {#if showToken}
          <div class="flex flex-col gap-3.5" transition:slide={fold}>
            <Field
              label="API token"
              type="password"
              autocomplete="current-password"
              spellcheck={false}
              placeholder="weg_…"
              bind:value={token}
              hint="Exchanged once for a session cookie a script on this page cannot read. The token itself is never stored in the browser."
            />
            <Button
              weight="primary"
              type="submit"
              disabled={session.busy || !ready}
              class="h-9 w-full justify-center"
            >
              {session.busy ? "Signing in…" : "Sign in"}
            </Button>
            <p class="text-xs text-ink-faint">
              No token yet? On the server, <code class="num text-ink-mute">weg token create</code>
              prints one. It is shown once, when it is created, and never again.
            </p>
          </div>
        {/if}
      </form>
    </div>
  </section>
</main>
