import { Skeleton } from "@/components/ui/skeleton";

// First-paint placeholder for the auth/boot gate — two quiet bars standing
// in for the app shell while /api/me resolves. Shared by the root page and
// AuthGuard so the two gates can't drift apart.
export function BootSkeleton() {
  return (
    <div className="flex min-h-dvh flex-col items-center justify-center gap-3 bg-background">
      <Skeleton className="h-9 w-40" />
      <Skeleton className="h-3.5 w-24" />
    </div>
  );
}
