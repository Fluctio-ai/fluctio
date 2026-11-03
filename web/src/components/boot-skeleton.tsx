import { LogoWave } from "@/components/logo-wave";

// First-paint placeholder for the auth/boot gate — the mark assembles on a
// loop while /api/me resolves. Shared by the root page and AuthGuard so the
// two gates can't drift apart.
export function BootSkeleton() {
  return (
    <div className="flex min-h-dvh items-center justify-center bg-background">
      <LogoWave size={72} loop />
    </div>
  );
}
