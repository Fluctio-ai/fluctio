"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { AlertCircle, Check, Loader2, Zap } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useT } from "@/lib/i18n";

// TestButton — pings a service with inline credentials and shows
// 测试 → spinner → ✓ 通过 / ✗ 失败 (+message). onTest resolves on success
// and throws on failure. Mirrors SaveButton's self-managing state so every
// "test connection" surface shares one look + feedback.
export function TestButton({
  onTest,
  disabled,
  className,
  label,
}: {
  onTest: () => Promise<void>;
  disabled?: boolean;
  className?: string;
  label?: string;
}) {
  const t = useT();
  const [state, setState] = useState<"idle" | "testing" | "ok" | "fail">("idle");
  const [msg, setMsg] = useState("");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(
    () => () => {
      if (timer.current) clearTimeout(timer.current);
    },
    [],
  );

  const handleClick = useCallback(async () => {
    setState("testing");
    try {
      await onTest();
      setState("ok");
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(() => setState("idle"), 3000);
    } catch (e) {
      setMsg(e instanceof Error ? e.message : "");
      setState("fail");
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(() => setState("idle"), 5000);
    }
  }, [onTest]);

  return (
    <Button
      type="button"
      variant="outline"
      size="sm"
      onClick={handleClick}
      disabled={state === "testing" || disabled}
      className={className}
    >
      {state === "ok" ? (
        <>
          <Check className="mr-1.5 h-3.5 w-3.5 text-success" />
          {t("common.testOk")}
        </>
      ) : state === "fail" ? (
        <>
          <AlertCircle className="mr-1.5 h-3.5 w-3.5 text-destructive" />
          {msg || t("common.testFailed")}
        </>
      ) : state === "testing" ? (
        <>
          <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" />
          {t("common.testing")}
        </>
      ) : (
        <>
          <Zap className="mr-1.5 h-3.5 w-3.5" />
          {label ?? t("common.test")}
        </>
      )}
    </Button>
  );
}
