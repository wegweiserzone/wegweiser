<script lang="ts">
  /** The signpost. One post, two boards, the near one carrying the accent. */
  let { class: extra = "" }: { class?: string } = $props();
</script>

<svg viewBox="0 0 24 24" fill="none" class={extra} aria-hidden="true">
  <path d="M12 2v20" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" />
  <path d="M12 5h7l2.5 3L19 11h-7z" fill="currentColor" />
  <path d="M12 13H5l-2.5 3L5 19h7z" fill="currentColor" opacity=".45" />
</svg>
