"use client";

import { cn } from "@/lib/utils";

// LogoWave — the inline, animated Fluctio mark. The three wave bands are
// one waveform: each band is the same path translated down the viewBox
// (the bands sit exactly 204.8 units apart, matching logo.svg), so the
// path data ships once. Bands live in their own <g> because the CSS
// drift animation (`.logo-wave path` in globals.css) owns the path
// transform — a transform attribute on the path itself would be
// overridden by it. Fills follow the theme's --brand token instead of a
// baked hex, so the mark re-colors with the surge-cyan axis per theme.
// Static surfaces CSS can't reach (favicon, PWA icons) keep logo.svg.
const WAVE_D =
  "M138.24 389.12c74.24 0 112.64-15.36 145.92-28.16 30.72-12.8 53.76-23.04 110.08-23.04 53.76 0 79.36 10.24 110.08 23.04 33.28 12.8 74.24 28.16 145.92 28.16 74.24 0 112.64-15.36 145.92-28.16 28.16-12.8 53.76-23.04 110.08-23.04v-102.4c-74.24 0-112.64 15.36-145.92 28.16-28.16 12.8-53.76 23.04-110.08 23.04-53.76 0-79.36-10.24-110.08-23.04-33.28-12.8-74.24-28.16-145.92-28.16s-112.64 15.36-145.92 28.16c-30.72 12.8-53.76 23.04-110.08 23.04v102.4z";
const BANDS = [
  { y: 0, opacity: 0.2 },
  { y: 204.8, opacity: 0.6 },
  { y: 409.6, opacity: 1 },
];

export function LogoWave({
  size = 32,
  className,
  title = "Fluctio",
}: {
  size?: number;
  className?: string;
  title?: string;
}) {
  return (
    <svg
      viewBox="0 0 1024 1024"
      width={size}
      height={size}
      role="img"
      aria-label={title}
      className={cn("logo-wave shrink-0", className)}
    >
      <g transform="translate(94.24 98.32) scale(0.8)">
        {BANDS.map((b) => (
          <g key={b.y} transform={`translate(0 ${b.y})`}>
            <path d={WAVE_D} fill="var(--brand)" opacity={b.opacity} />
          </g>
        ))}
      </g>
    </svg>
  );
}
