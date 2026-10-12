<script lang="ts">
  /**
   * What the server answers for a question, decided by the function the query
   * path uses (D49). The page a person opens instead of reaching for dig, and
   * one they can walk: every name in an answer asks about itself.
   */
  import { untrack } from "svelte";

  import { replaceState } from "$app/navigation";
  import { page } from "$app/state";
  import { api, ApiError, NetworkError } from "$lib/api";
  import type { AnsweredRecord, Asked } from "$lib/api";
  import { asText, describe, headline, target, tone } from "$lib/ask";
  import Bar from "$lib/components/Bar.svelte";
  import Button from "$lib/components/Button.svelte";
  import Empty from "$lib/components/Empty.svelte";
  import Notice from "$lib/components/Notice.svelte";
  import TypeField from "$lib/components/TypeField.svelte";

  let name = $state(page.url.searchParams.get("name") ?? "");
  let type = $state((page.url.searchParams.get("type") ?? "A").toUpperCase());

  let asked = $state<Asked | null>(null);
  let asking = $state(false);
  let refused = $state<string | null>(null);

  /** The questions asked here, newest first, so a comparison is one click. */
  let recent = $state<Asked[]>(remembered());

  function remembered(): Asked[] {
    try {
      return JSON.parse(sessionStorage.getItem("weg.ask.recent") ?? "[]") as Asked[];
    } catch {
      return [];
    }
  }

  function remember(a: Asked) {
    recent = [a, ...recent.filter((r) => r.name !== a.name || r.type !== a.type)].slice(0, 12);
    try {
      sessionStorage.setItem("weg.ask.recent", JSON.stringify(recent));
    } catch {
      // A browser that keeps nothing still asks; it forgets the list on reload.
    }
  }

  async function ask(n = name, t = type) {
    const q = n.trim();
    if (!q) return;
    name = q;
    type = (t.trim() || "A").toUpperCase();
    asking = true;
    refused = null;
    try {
      asked = await api.get("/ask", { query: { name: q, type } });
      remember(asked);
      // The question is in the address, so it can be sent to somebody else.
      const url = new URL(page.url);
      url.searchParams.set("name", q);
      url.searchParams.set("type", type);
      replaceState(url, page.state);
    } catch (err) {
      asked = null;
      refused =
        err instanceof ApiError
          ? (err.detail ?? err.title)
          : err instanceof NetworkError
            ? "The server did not answer."
            : "The question could not be asked.";
    } finally {
      asking = false;
    }
  }

  // Arriving with a question in the address asks it.
  $effect(() => {
    untrack(() => {
      if (name) void ask();
    });
  });

  const sections = $derived(
    asked
      ? [
          { title: "Answer", rows: asked.answer },
          { title: "Authority", rows: asked.authority },
          { title: "Additional", rows: asked.additional },
        ].filter((s) => s.rows.length > 0)
      : [],
  );

  /** What the answer holds, as a sentence of its own under the headline. */
  const told = $derived.by(() => {
    const said = asked ? describe(asked) : "";
    // Not capitalised here: a sentence may start with a domain name.
    return !said || /[.!?]$/.test(said) ? said : `${said}.`;
  });

  const ink = { neutral: "text-ink", warn: "text-warn", crit: "text-crit" } as const;

  /** follow asks about a name an answer named: its own type for an alias, A otherwise. */
  function follow(r: AnsweredRecord) {
    const next = target(r);
    if (next) void ask(next, r.type === "CNAME" || r.type === "DNAME" ? type : "A");
  }
</script>

<svelte:head><title>Ask — Wegweiser</title></svelte:head>

<Bar title="Ask" />

<form
  class="flex shrink-0 flex-wrap items-end gap-3 border-b border-line bg-surface px-5 py-3"
  onsubmit={(e) => {
    e.preventDefault();
    void ask();
  }}
>
  <label class="flex min-w-[16rem] flex-1 flex-col gap-1.5">
    <span class="sign text-[11px] text-ink-faint">Name</span>
    <input
      bind:value={name}
      placeholder="www.example.com"
      aria-label="Name to ask about"
      autocomplete="off"
      spellcheck="false"
      class="num h-9 rounded-sm border border-line bg-ground px-3 text-[14px] text-ink
             outline-none placeholder:text-ink-faint focus:border-signal"
    />
  </label>
  <div class="w-40">
    <TypeField bind:value={type} id="ask-type" />
  </div>
  <Button weight="primary" type="submit" disabled={asking || !name.trim()}>
    {asking ? "Asking…" : "Ask"}
  </Button>
</form>

<div class="grid min-h-0 flex-1 overflow-auto lg:grid-cols-[minmax(0,1fr)_17rem]">
  <div class="flex min-w-0 flex-col gap-5 py-5">
    {#if refused}
      <div class="px-5">
        <Notice tone="crit" title="Not asked">{refused}</Notice>
      </div>
    {:else if !asked}
      <div class="px-5 py-12">
        <Empty title="Ask what this server answers">
          A name and a type, and the answer comes from the data this server is serving, by
          the same code that answers queries: no <code class="num text-ink">dig</code>, and no
          route to the DNS port needed. Every name in an answer can be asked about in turn.
        </Empty>
      </div>
    {:else}
      <!-- The answer first, in words; the records it is made of below. -->
      <section class="px-5" aria-label="The answer">
        <p
          class="font-cond text-[24px] leading-none font-bold tracking-[0.06em] uppercase
                 {ink[tone[asked.outcome]]}"
        >
          {headline[asked.outcome]}
        </p>
        <p class="mt-2 text-[13px] text-ink-mute">
          <span class="num text-ink">{asked.rcode}</span>{asked.authoritative
            ? ", authoritative"
            : ""}{#if asked.zone}, from
            <a
              href="/zones/{encodeURIComponent(asked.zone)}"
              class="num text-ink underline-offset-2 hover:text-signal hover:underline"
              >{asked.zone}</a
            >{/if}
        </p>
        {#if told}
          <p class="mt-1 text-[13px] text-ink-mute">{told}</p>
        {/if}
      </section>

      {#if sections.length > 0}
        <!-- One table for the three sections, so the columns line up across
             them the way dig's do; a section is a row of its own. -->
        <table class="w-full border-collapse text-[13px]" data-hide="2">
          {#each sections as section (section.title)}
            <tbody aria-label={section.title}>
              <tr>
                <th
                  colspan="4"
                  scope="colgroup"
                  class="sign px-5 pt-3 pb-1.5 text-left text-[11px] font-normal text-ink-faint"
                >
                  {section.title}
                </th>
              </tr>
              {#each section.rows as r, i (i)}
                {@const next = target(r)}
                <tr class="border-b border-line-soft">
                  <td class="num py-1.5 pr-3 pl-5 align-top">
                    <button
                      type="button"
                      onclick={() => ask(r.name, r.type)}
                      aria-label="Ask about {r.name} {r.type}"
                      title="Ask about {r.name} {r.type}"
                      class="cursor-pointer text-left hover:text-signal"
                    >
                      {r.name}
                    </button>
                  </td>
                  <td class="num w-20 px-3 py-1.5 text-right align-top text-ink-faint">{r.ttl}</td>
                  <td class="num w-20 px-3 py-1.5 align-top text-[12px] font-medium">{r.type}</td>
                  <td class="num px-3 py-1.5 pr-5 align-top [overflow-wrap:anywhere]">
                    {#if next}
                      <button
                        type="button"
                        onclick={() => follow(r)}
                        aria-label="Ask about {next}"
                        title="Ask about {next}"
                        class="cursor-pointer text-left hover:text-signal"
                      >
                        {r.data}
                      </button>
                    {:else}
                      {r.data}
                    {/if}
                  </td>
                </tr>
              {/each}
            </tbody>
          {/each}
        </table>
      {/if}

      <details class="px-5">
        <summary class="sign cursor-pointer text-[11px] text-ink-faint hover:text-ink">
          As a zonefile writes it
        </summary>
        <pre
          class="num mt-2 overflow-x-auto rounded-sm border border-line bg-surface p-3
                 text-[12px] text-ink-mute">{asText(asked)}</pre>
      </details>
    {/if}
  </div>

  <aside class="border-line px-5 py-5 lg:border-l" aria-label="Asked before">
    <h2 class="sign pb-2 text-[11px] text-ink-faint">Asked before</h2>
    {#each recent as r (r.name + r.type)}
      <button
        type="button"
        onclick={() => ask(r.name, r.type)}
        class="flex w-full cursor-pointer items-baseline gap-2 rounded-sm px-2 py-1 text-left
               text-[12px] hover:bg-raised"
      >
        <span class="num min-w-0 flex-1 truncate">{r.name}</span>
        <span class="num text-ink-faint">{r.type}</span>
        <span class="num text-[11px] {ink[tone[r.outcome]]}">{r.rcode}</span>
      </button>
    {:else}
      <p class="text-[12px] text-ink-faint">Nothing yet in this tab.</p>
    {/each}
  </aside>
</div>
