<script lang="ts">
  /**
   * What stands where a change would be, when no change can land from here: a
   * session that may only read, or a cluster that takes no writes. A control
   * that quietly disappears teaches the wrong shape of the system.
   */
  import { standing } from "$lib/standing.svelte";
  import Chip from "./Chip.svelte";

  let { class: extra = "" }: { class?: string } = $props();
</script>

<span class={extra}>
  <Chip title={standing.writes.reason}>{standing.withheld}</Chip>
</span>
