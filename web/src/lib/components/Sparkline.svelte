<script lang="ts">
  /**
   * Queries per second, over the last minute.
   */
  let {
    series,
    label,
  }: {
    /** One reading a step, oldest first; null for a step from before anything was measured. */
    series: (number | null)[];
    label: string;
  } = $props();

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

    const peak = Math.max(10, ...series.map((v) => v ?? 0));
    const step = box.width / Math.max(1, series.length - 1);
    const y = (v: number) => box.height - 4 - (v / peak) * (box.height - 12);

    c.strokeStyle = token("--line-soft");
    c.lineWidth = 1;
    for (let i = 1; i <= 2; i++) {
      const gy = Math.round((box.height * i) / 3) + 0.5;
      c.beginPath();
      c.moveTo(0, gy);
      c.lineTo(box.width, gy);
      c.stroke();
    }

    // The line starts where measuring did. Before the page was open nothing
    // was counted, and drawing that time as zero would claim a quiet minute
    // nobody saw.
    const first = series.findIndex((v) => v !== null);
    if (first === -1) return;

    const line = new Path2D();
    for (let i = first; i < series.length; i++) {
      const at = [i * step, y(series[i] ?? 0)] as const;
      if (i === first) line.moveTo(...at);
      else line.lineTo(...at);
    }

    const under = new Path2D(line);
    under.lineTo(box.width, box.height);
    under.lineTo(first * step, box.height);
    under.closePath();

    const signal = token("--signal");
    const fade = c.createLinearGradient(0, 0, 0, box.height);
    fade.addColorStop(0, `color-mix(in srgb, ${signal} 34%, transparent)`);
    fade.addColorStop(1, `color-mix(in srgb, ${signal} 0%, transparent)`);
    c.fillStyle = fade;
    c.fill(under);

    c.strokeStyle = signal;
    c.lineWidth = 1.5;
    c.lineJoin = "round";
    c.stroke(line);

    // The newest second, marked: this is a live chart and the eye should know
    // which end it is arriving at.
    const latest = series.at(-1) ?? 0;
    c.fillStyle = signal;
    c.beginPath();
    c.arc(box.width - 1.5, y(latest), 2.5, 0, Math.PI * 2);
    c.fill();
  }

  $effect(() => {
    // Reading the series is what makes this redraw when a second passes.
    void series;
    draw();
  });

  $effect(() => {
    const observer = new ResizeObserver(() => draw());
    if (canvas) observer.observe(canvas);
    return () => observer.disconnect();
  });
</script>

<canvas bind:this={canvas} class="block h-14 w-full" aria-label={label}></canvas>
