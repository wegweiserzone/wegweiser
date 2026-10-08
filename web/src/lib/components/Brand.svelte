<script lang="ts">
  /**
   * The product's name, and under it the one line about how this server
   * stands that belongs on every page: the rail's head on a wide screen, the
   * bar across the top on a narrow one.
   */
  import { standing } from "$lib/standing.svelte";
  import Mark from "./Mark.svelte";

  const health = $derived(standing.health);
  /** trouble is the cluster's answer when it is anything but "takes writes". */
  const trouble = $derived(standing.verdict?.tone === "ok" ? null : standing.verdict);
  /** short says it in the width there is. */
  const short: Record<string, string> = {
    "One failure from stopping": "no voter to spare",
  };
</script>

<div class="flex min-w-0 items-center gap-2.5">
  <Mark class="size-5.5 shrink-0 text-signal" />
  <div class="min-w-0">
    <p class="font-cond text-[19px] leading-none font-bold tracking-[0.13em] uppercase">
      Wegweiser
    </p>
    <!--
      A member that has left its cluster still serves, so the version line is
      where it says so: everywhere, rather than only on the page that explains
      it (docs/decisions/d29-a-node-that-cannot-apply.md,
      docs/decisions/d46-a-member-that-has-left.md). A cluster close to taking
      no writes, or taking none, says so in the same place for the same reason.
    -->
    {#if health?.current === false}
      <a href="/cluster" class="num mt-0.5 block truncate text-[10px] text-crit">
        left its cluster
      </a>
    {:else if trouble}
      <a
        href="/cluster"
        title={trouble.detail}
        class="num mt-0.5 block truncate text-[10px] {trouble.tone === 'crit'
          ? 'text-crit'
          : 'text-warn'}"
      >
        {short[trouble.headline] ?? trouble.headline.toLowerCase()}
      </a>
    {:else}
      <p class="num mt-0.5 truncate text-[10px] text-ink-faint">
        {health?.version ?? "not serving"}
      </p>
    {/if}
  </div>
</div>
