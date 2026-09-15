"use client";

import WAVES from "@/components/logo-waves.gen";
import { cn } from "@/lib/utils";

// LogoWave — the inline, animated Fluctio mark. Same three wave bands and
// safe-area group as public/logo.svg, but inline so CSS can phase-shift
// each band (`.logo-wave path` in globals.css) and the fills can follow
// the theme's --brand token instead of a baked hex. Falls back to the
// static logo everywhere animation isn't wanted (reduced-motion users
// get the still image via the CSS media query).
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
        {WAVES.map((w, i) => (
          <path key={i} d={w.d} fill="var(--brand)" opacity={w.opacity} />
        ))}
      </g>
    </svg>
  );
}
