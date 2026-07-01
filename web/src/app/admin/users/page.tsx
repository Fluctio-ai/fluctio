"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";

// Single-user mode: the multi-user management console (user CRUD +
// registration toggle) has been removed. The platform serves exactly one
// owner, created via onboarding or `fastclaw admin create-user`.
// /admin/users redirects to the overview.
export default function AdminUsersRedirect() {
  const router = useRouter();
  useEffect(() => {
    router.replace("/overview/");
  }, [router]);
  return null;
}
