"use client";

import { useState } from "react";
import { useT } from "@/lib/i18n";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Loader2, Check, FlaskConical } from "lucide-react";
import { testEmbedding, testReranker } from "@/lib/api";

type Status = "idle" | "testing" | "success" | "error";

interface MemoryTestButtonProps {
  kind: "embedding" | "reranker";
  apiBase: string;
  apiKey: string;
  model: string;
  dim?: number;
  dimEnabled?: boolean;
}

// MemoryTestButton pings the configured embedding or reranker endpoint
// with the form's inline credentials so the operator can verify the
// apiBase / apiKey / model are correct before saving. Renders a Test
// button + a status badge (spinner / connected / failed) mirroring the
// Models page's per-model test affordance.
export function MemoryTestButton({
  kind,
  apiBase,
  apiKey,
  model,
  dim,
  dimEnabled,
}: MemoryTestButtonProps) {
  const t = useT();
  const [status, setStatus] = useState<Status>("idle");
  const [error, setError] = useState<string | null>(null);
  const [dimResult, setDimResult] = useState<number | null>(null);

  const handleTest = async () => {
    setStatus("testing");
    setError(null);
    setDimResult(null);
    try {
      // Branch per kind so the union return type narrows cleanly — the
      // embedding call carries `dim`, the reranker call doesn't.
      let ok = false;
      let errMsg: string | undefined;
      if (kind === "embedding") {
        const r = await testEmbedding({ apiBase, apiKey, model, dim, dimEnabled });
        ok = r.ok;
        errMsg = r.error;
        if (ok && typeof r.dim === "number") setDimResult(r.dim);
      } else {
        const r = await testReranker({ apiBase, apiKey, model });
        ok = r.ok;
        errMsg = r.error;
      }
      if (ok) {
        setStatus("success");
      } else {
        setStatus("error");
        setError(errMsg || t("models.connectionFailed"));
      }
    } catch (e) {
      setStatus("error");
      setError(e instanceof Error ? e.message : t("models.connectionFailed"));
    }
  };

  return (
    <div className="flex items-center gap-3">
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={handleTest}
        disabled={status === "testing" || !apiBase || !apiKey || !model}
      >
        {status === "testing" ? (
          <Loader2 className="h-3 w-3 mr-1.5 animate-spin" />
        ) : (
          <FlaskConical className="h-3 w-3 mr-1.5" />
        )}
        {status === "testing" ? t("models.testing") : t("models.testConnection")}
      </Button>
      {status === "success" && (
        <Badge className="bg-success/15 text-success hover:bg-success/15 text-[10px]">
          <Check className="mr-1 size-3" />
          {t("models.connected")}
          {dimResult ? ` · ${dimResult}d` : ""}
        </Badge>
      )}
      {status === "error" && (
        <Badge
          variant="outline"
          className="border-destructive/40 text-destructive text-[10px] max-w-[260px] truncate"
          title={error || undefined}
        >
          {t("models.failed")}: {error}
        </Badge>
      )}
    </div>
  );
}
