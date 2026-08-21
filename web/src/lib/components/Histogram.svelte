<script lang="ts">
  /**
   * Where the latencies fall.
   */
  let { buckets, labels }: { buckets: number[]; labels: string[] } = $props();

  let canvas = $state<HTMLCanvasElement | null>(null);

  function token(name: string): string {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }

  function draw() {
    if (!canvas) return;
    const box = canvas.getBoundingClientRect();
    if (!box.width) return;

    const ratio = Math.min(devicePixelRatio || 1, 2);
    canvas.width = Math.round(box.width * ratio);
    canvas.height = Math.round(box.height * ratio);

    const c = canvas.getContext("2d");
    if (!c) return;
    c.setTransform(ratio, 0, 0, ratio, 0, 0);
    c.clearRect(0, 0, box.width, box.height);

    const peak = Math.max(1, ...buckets);
    const gap = 5;
    const width = (box.width - gap * (buckets.length - 1)) / buckets.length;
    const floor = box.height - 13;

    c.font = '9px "JetBrains Mono", ui-monospace, monospace';
    c.textAlign = "center";

    buckets.forEach((count, i) => {
      const height = count === 0 ? 0 : Math.max(2, (count / peak) * (floor - 2));
      const x = i * (width + gap);

      // The colour is the verdict: the first buckets are what this server is
      // for, the last ones are what somebody should look at.
      c.fillStyle =
        i <= 1 ? token("--signal") : i <= 3 ? token("--ink-mute") : i === 4 ? token("--warn") : token("--crit");
      c.globalAlpha = i <= 1 ? 0.95 : 0.65;
      c.fillRect(x, floor - height, width, height);
      c.globalAlpha = 1;

      c.fillStyle = token("--ink-faint");
      c.fillText(labels[i] ?? "", x + width / 2, box.height - 3);
    });
  }

  $effect(() => {
    void buckets;
    draw();
  });

  $effect(() => {
    const observer = new ResizeObserver(() => draw());
    if (canvas) observer.observe(canvas);
    return () => observer.disconnect();
  });
</script>

<canvas bind:this={canvas} class="block h-14 w-full" aria-label="Latency distribution"></canvas>
