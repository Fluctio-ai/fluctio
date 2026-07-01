"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";

// Single-user mode: self-registration has been removed. /signup redirects
// to / (the owner signs in; the account is created via onboarding or
// `fastclaw admin create-user`).
export default function SignupRedirect() {
  const router = useRouter();
  useEffect(() => {
    router.replace("/");
  }, [router]);
  return null;
}
