"use client";

import { cn } from "@/lib/utils";
import { MARK_LAYERS } from "./logo-mark-data";

// LogoWave — the inline Fluctio mark, identical to the launcher icon
// (horizontally mirrored, then 16 deg counter-clockwise). The artwork is
// split into its five paint-order layers; each layer rotates clockwise
// into place with a damped settle, so the mark assembles like successive
// surges. The layer data is generated from tool/brand/mark.svg by
// tool/build_brand_assets.py (paint order; delayMs = reveal order).
export function LogoWave({
  size = 32,
  className,
  title = "Fluctio",
  loop = false,
}: {
  size?: number;
  className?: string;
  title?: string;
  loop?: boolean;
}) {
  return (
    <svg
      viewBox="0 0 1025 1024"
      width={size}
      height={size}
      role="img"
      aria-label={title}
      className={cn("logo-wave shrink-0", loop && "logo-wave--loop", className)}
    >
      <g transform="rotate(-16 512.5 512) translate(1025 0) scale(-1 1)">
        {/* Darkest tier: a full disc settles first, then the lighter tiers
            sweep clockwise into place, darkest to lightest (white last).
            Each tier also paints its colour under every later-painted tier
            (their shapes are pre-cut in the artwork), so no notch shows
            while a lighter tier is still in flight. Later tiers repaint
            those areas, so the settled mark is unchanged. */}
        <g className="logo-base">
          <circle cx="512.5" cy="512" r="511.5" fill={MARK_LAYERS[0].fill} />
          {MARK_LAYERS[0].d.map((d, j) => (
            <path key={j} d={d} fill={MARK_LAYERS[0].fill} />
          ))}
        </g>
        {MARK_LAYERS.slice(1).map((layer, i) => (
          <g
            key={layer.fill}
            className="logo-layer"
            style={{ animationDelay: `${layer.delayMs}ms` }}
          >
            {[layer, ...MARK_LAYERS.slice(i + 2)].flatMap((under, u) =>
              under.d.map((d, j) => (
                <path key={`${u}-${j}`} d={d} fill={layer.fill} />
              )),
            )}
          </g>
        ))}
      </g>
    </svg>
  );
}
