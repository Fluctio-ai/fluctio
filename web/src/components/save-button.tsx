"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { AlertCircle, Check, Loader2, Save } from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { useT } from "@/lib/i18n";

// SaveButton — the standard save control for settings/config surfaces.
// Self-manages idle → saving → saved (green Check, 2s) / error (red, 3s) with
// an icon at every state. `onSave` is async; throw inside it to trigger the
// error state. Centralized so every save surface shares one look + feedback
// (pages previously differed on icon presence and success/error feedback).
export function SaveButton({
  onSave,
  disabled,
  size,
  className,
  label,
  savedLabel,
  errorLabel,
}: {
  onSave: () => Promise<void>;
  disabled?: boolean;
  size?: "default" | "sm" | "lg" | "icon";
  className?: string;
  label?: string;
  savedLabel?: string;
  errorLabel?: string;
}) {
  const t = useT();
  const [state, setState] = useState<"idle" | "saving" | "saved" | "error">("idle");
  const [errMsg, setErrMsg] = useState("");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(
    () => () => {
      if (timer.current) clearTimeout(timer.current);
    },
    [],
  );

  const handleClick = useCallback(async () => {
    setState("saving");
    try {
      await onSave();
      setState("saved");
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(() => setState("idle"), 2000);
    } catch (e) {
      setErrMsg(e instanceof Error ? e.message : "");
      setState("error");
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(() => setState("idle"), 3000);
    }
  }, [onSave]);

  return (
    <Button
      type="button"
      onClick={handleClick}
      disabled={state === "saving" || disabled}
      variant={state === "saved" || state === "error" ? "outline" : "default"}
      size={size}
      className={cn(
        state === "saved" && "border-success/30 text-success",
        state === "error" && "border-destructive/40 text-destructive",
        className,
      )}
    >
      {state === "saved" ? (
        <>
          <Check className="h-4 w-4 mr-2" />
          {savedLabel ?? t("common.saved")}
        </>
      ) : state === "saving" ? (
        <>
          <Loader2 className="h-4 w-4 mr-2 animate-spin" />
          {t("common.saving")}
        </>
      ) : state === "error" ? (
        <>
          <AlertCircle className="h-4 w-4 mr-2" />
          {errMsg || errorLabel || t("common.saveFailed")}
        </>
      ) : (
        <>
          <Save className="h-4 w-4 mr-2" />
          {label ?? t("common.save")}
        </>
      )}
    </Button>
  );
}
