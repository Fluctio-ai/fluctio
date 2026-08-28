import type { KeyboardEvent } from "react";
import { SearchIcon } from "lucide-react";

import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

// SearchInput is the icon-prefixed search box shared by the KB/wiki view
// toolbars — one definition so icon size, padding, and input styling stay
// in sync across views. onBlur/onKeyDown are optional pass-throughs for
// the views that trigger a reload on blur/Enter.
export function SearchInput({
  value,
  onChange,
  placeholder,
  className,
  onBlur,
  onKeyDown,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  className?: string;
  onBlur?: () => void;
  onKeyDown?: (e: KeyboardEvent<HTMLInputElement>) => void;
}) {
  return (
    <div className={cn("relative min-w-0 flex-1", className)}>
      <SearchIcon className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
      <Input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        onBlur={onBlur}
        onKeyDown={onKeyDown}
        placeholder={placeholder}
        className="h-7 w-full rounded-md pl-8 text-xs"
      />
    </div>
  );
}
