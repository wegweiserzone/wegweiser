<script lang="ts">
  /**
   * The one button. Three weights: primary for the action a screen exists for,
   * default for everything else, quiet for the way out.
   */
  import type { Snippet } from "svelte";
  import type { HTMLButtonAttributes } from "svelte/elements";

  type Weight = "primary" | "default" | "quiet";

  interface Props extends HTMLButtonAttributes {
    weight?: Weight;
    children: Snippet;
  }

  let { weight = "default", children, class: extra = "", ...rest }: Props = $props();

  const weights: Record<Weight, string> = {
    primary: "border-signal bg-signal text-signal-on hover:border-signal-hi hover:bg-signal-hi",
    default: "border-line bg-surface text-ink hover:border-ink-faint hover:bg-raised",
    quiet: "border-transparent bg-transparent text-ink-mute hover:bg-raised hover:text-ink",
  };
</script>

<button
  class="sign inline-flex h-8 cursor-pointer items-center gap-1.5 rounded-sm border px-3
         text-[13px] whitespace-nowrap transition-colors {weights[weight]} {extra}
         disabled:cursor-not-allowed disabled:border-line disabled:bg-raised
         disabled:text-ink-faint disabled:hover:border-line disabled:hover:bg-raised"
  {...rest}
>
  {@render children()}
</button>
