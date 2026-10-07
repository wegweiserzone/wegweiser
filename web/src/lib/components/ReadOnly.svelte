<script lang="ts">
  /**
   * What stands where a change would be, for a session that may only read. A
   * control that quietly disappears teaches the wrong shape of the system.
   */
  import Chip from "./Chip.svelte";

  let { class: extra = "" }: { class?: string } = $props();
</script>

<span class={extra}>
  <Chip title="Changing this needs a token with the write scope.">Read only</Chip>
</span>
