"use client";

import * as React from "react";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

// Settings form primitives — the single layout dialect for every settings
// surface (user-level /settings/*, agent tabs in AgentSettingsDialog, and
// the Knowledge cards). Before this module the pages grew four page-header
// sizes, three field densities and as many save-button dialects; the parts
// below encode the one grammar:
//
//   PageHeader   h2 text-xl + desc + right-aligned actions
//   SettingsCard rounded-lg border bg-card (padded via `padded`)
//   CardHead     icon + text-sm title + badge, desc below, control right
//   Field        Label text-sm + control + text-xs hint, space-y-1.5
//   NumberField  typeable numeric Input (draft-while-focused, commit on blur)
//   ToggleRow    title/hint left, Switch right
//   GroupLabel   text-xs muted-medium group label inside a card body
//   SettingsError the one error-banner dialect
//
// Control rhythm: every control stays on the design system's default h-8
// line — no text-xs inputs, no h-7 minis. Label size encodes hierarchy:
// text-sm for fields, GroupLabel for in-card groups.

export function PageHeader({
  title,
  desc,
  actions,
  className,
}: {
  title: React.ReactNode;
  desc?: React.ReactNode;
  actions?: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("flex items-start justify-between gap-4", className)}>
      <div className="min-w-0">
        <h2 className="text-xl font-semibold tracking-tight">{title}</h2>
        {desc && <p className="mt-1 text-sm text-muted-foreground">{desc}</p>}
      </div>
      {actions && (
        <div className="flex shrink-0 items-center gap-2 pt-0.5">{actions}</div>
      )}
    </div>
  );
}

export function SettingsCard({
  className,
  children,
  // Sectioned cards (runtime, about) pass padded={false} and own their
  // `p-5` per section; everything else gets it here.
  padded = true,
}: {
  className?: string;
  children: React.ReactNode;
  padded?: boolean;
}) {
  return (
    <div
      className={cn(
        "rounded-lg border border-border bg-card",
        padded && "p-5",
        className,
      )}
    >
      {children}
    </div>
  );
}

export function CardHead({
  icon: Icon,
  title,
  badge,
  desc,
  control,
  className,
}: {
  icon?: React.ComponentType<{ className?: string }>;
  title: React.ReactNode;
  badge?: React.ReactNode;
  desc?: React.ReactNode;
  control?: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("flex items-start justify-between gap-3", className)}>
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          {Icon && <Icon className="size-4 shrink-0 text-primary" />}
          <h3 className="text-sm font-medium leading-none">{title}</h3>
          {badge}
        </div>
        {desc && (
          <p className="mt-1.5 text-sm leading-relaxed text-muted-foreground">
            {desc}
          </p>
        )}
      </div>
      {control && <div className="shrink-0 pt-0.5">{control}</div>}
    </div>
  );
}

export function Field({
  label,
  htmlFor,
  hint,
  // Right-aligned slot on the label row — live values for range inputs
  // (percentages, thresholds). Keeps slider rows on the Field primitive
  // instead of hand-rolled label-row JSX.
  labelTrailing,
  children,
  className,
}: {
  label?: React.ReactNode;
  htmlFor?: string;
  hint?: React.ReactNode;
  labelTrailing?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("space-y-1.5", className)}>
      {(label || labelTrailing) && (
        <div className="flex items-center justify-between gap-2">
          {label && <Label htmlFor={htmlFor}>{label}</Label>}
          {labelTrailing && (
            <span className="text-xs text-muted-foreground tabular-nums">
              {labelTrailing}
            </span>
          )}
        </div>
      )}
      {children}
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
}

// NumberField — typeable numeric Input (settings dialect's only numeric
// control). `type="number"` + re-parsing the controlled value per keystroke
// makes decimals untypeable ("0." collapses back to "0"), so this edits a
// local string draft and commits once on blur: empty or unparseable drafts
// fall back to the current value, anything else is clamped to min/max.
// inputMode="decimal" keeps phones on the decimal pad without native spinners.
export function NumberField({
  value,
  onChange,
  min,
  max,
  ...props
}: {
  value: number;
  onChange: (n: number) => void;
  min?: number;
  max?: number;
} & Omit<
  React.ComponentProps<typeof Input>,
  "value" | "onChange" | "type" | "inputMode" | "min" | "max" | "step"
>) {
  // draft === null ⇒ not editing: render the canonical value so outside
  // updates (config reload after save) always show through. The draft is
  // seeded implicitly by the first keystroke — the input event carries the
  // full text (canonical value + edit), so no onFocus reset is needed.
  const [draft, setDraft] = React.useState<string | null>(null);
  const commit = () => {
    const raw = draft;
    setDraft(null);
    if (raw === null) return;
    const n = Number(raw);
    if (raw.trim() === "" || !Number.isFinite(n)) return;
    const next = Math.min(max ?? Infinity, Math.max(min ?? -Infinity, n));
    if (next !== value) onChange(next);
  };
  return (
    <Input
      type="text"
      inputMode="decimal"
      value={draft ?? String(value)}
      onChange={(e) => setDraft(e.target.value)}
      onBlur={commit}
      {...props}
    />
  );
}

export function ToggleRow({
  title,
  hint,
  checked,
  onCheckedChange,
  disabled,
  id,
  className,
}: {
  title: React.ReactNode;
  hint?: React.ReactNode;
  checked: boolean;
  onCheckedChange: (v: boolean) => void;
  disabled?: boolean;
  id?: string;
  className?: string;
}) {
  return (
    <div className={cn("flex items-start justify-between gap-4", className)}>
      <div className="min-w-0 space-y-0.5">
        <div className="text-sm font-medium leading-none">{title}</div>
        {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
      </div>
      <Switch
        id={id}
        className="mt-0.5 shrink-0"
        checked={checked}
        onCheckedChange={onCheckedChange}
        disabled={disabled}
      />
    </div>
  );
}

export function GroupLabel({
  className,
  children,
}: {
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div className={cn("text-xs font-medium text-muted-foreground", className)}>
      {children}
    </div>
  );
}

// GroupHead — a GroupLabel with optional desc and a right-aligned control
// (usually a Switch). The group-level twin of CardHead: sub-sections inside
// a card whose enable toggle belongs on the group title line (e.g. 闪存召回
// inside the KB card) use this instead of stacking label/desc/switch as
// three separate rows.
export function GroupHead({
  title,
  desc,
  control,
  className,
}: {
  title: React.ReactNode;
  desc?: React.ReactNode;
  control?: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("flex items-start justify-between gap-4", className)}>
      <div className="min-w-0 space-y-0.5">
        <GroupLabel>{title}</GroupLabel>
        {desc && (
          <p className="text-xs leading-relaxed text-muted-foreground">{desc}</p>
        )}
      </div>
      {control && <div className="shrink-0 pt-0.5">{control}</div>}
    </div>
  );
}

export function SettingsError({
  className,
  children,
}: {
  className?: string;
  children: React.ReactNode;
}) {
  if (!children) return null;
  return (
    <div
      role="alert"
      className={cn(
        "rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive",
        className,
      )}
    >
      {children}
    </div>
  );
}
