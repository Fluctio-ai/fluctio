// Shared FastClaw-style tile classes for the sidebar's compact grids
// (NavMain `tiles` mode + NavKnowledge). Cells stack icon-over-label
// vertically inside a bordered muted container; hover lifts the tile
// out of the container (translate + shadow), mirroring the quick-action
// grid in fastclaw-ai/fastclaw's BotControlPanel. Classes are plain
// strings so callers merge them into SidebarMenuButton's cn() chain,
// overriding its horizontal row defaults via tailwind-merge.
//
// Container base: callers append their literal column count
// (`grid-cols-2` for the two-entry agent group, `grid-cols-3` for the
// eight KB entries) — literals stay in source so Tailwind v4 scanning
// emits them. Both hide under the collapsed icon rail — a tile grid has
// no meaningful single-icon form (the tiles' min-height and vertical
// stack would overflow the 32px rail), matching NavKnowledge's
// whole-group hide.
export const navTileGrid =
  "grid gap-1 rounded-lg border border-sidebar-border bg-sidebar-accent/40 p-1 group-data-[collapsible=icon]:hidden";

// Cell button: flex-col overrides the base row layout; [&_svg]:size-[18px]
// must live here (not on the icon element) because the base button's
// `[&_svg]:size-4` descendant selector would otherwise win specificity.
export const navTileButton =
  "h-auto min-h-[60px] w-full flex-col items-center justify-center gap-1.5 px-1.5 py-2 transition-[background-color,color,box-shadow,transform] hover:-translate-y-px hover:bg-background hover:text-foreground hover:shadow-sm active:translate-y-0 [&_svg]:size-[18px]";

// Label under the icon — truncation comes free from the base button's
// `[&>span:last-child]:truncate`.
export const navTileLabel = "max-w-full text-xs font-medium leading-4";

// Icon polish: subtle zoom while the tile lifts. Applied to the icon
// element itself (group-hover targets the button's own group/menu-button).
export const navTileIcon = "shrink-0 transition-transform group-hover/menu-button:scale-105";
