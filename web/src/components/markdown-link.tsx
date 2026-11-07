"use client";

import type { AnchorHTMLAttributes } from "react";

/**
 * ExternalAnchor renders <a> for ReactMarkdown chat content with one tweak:
 * links pointing at a different origin open in a new tab. Same-origin / relative
 * / mailto / # links keep default behaviour.
 *
 * Why: chat replies routinely include outbound URLs (Namecheap, GitHub, docs).
 * Opening those in the current tab navigates away from the chat session and
 * loses the agent's context. Forcing target="_blank" only on cross-origin
 * URLs avoids breaking in-app navigation (we still rely on the Next.js router
 * for /agents/<id>/... links the agent might emit).
 *
 * Pair with rel="noopener noreferrer" so the popup can't reach back to
 * window.opener — standard hardening for any anchor that opens a new tab.
 */
export function ExternalAnchor(props: AnchorHTMLAttributes<HTMLAnchorElement>) {
  const { href, children, ...rest } = props;
  // Protocol gate: content is LLM/tool output, so a scheme outside the
  // safe set (javascript:, data:, vbscript:, app:, …) must not become a
  // clickable anchor. Render the label as plain text instead — react-
  // markdown's default urlTransform filters at ITS layer, but this
  // component is shared with wiki/doc renders whose transforms differ.
  if (hasUnsafeProtocol(href)) {
    return <span {...rest}>{children}</span>;
  }
  const external = isExternalHref(href);
  if (external) {
    return (
      <a href={href} target="_blank" rel="noopener noreferrer" {...rest}>
        {children}
      </a>
    );
  }
  return (
    <a href={href} {...rest}>
      {children}
    </a>
  );
}

// hasUnsafeProtocol reports whether href carries an explicit scheme that
// is not in the renderable set (http/https/mailto/tel). Relative paths,
// root-relative, hash anchors, and scheme-less values are safe by
// definition — the browser resolves them against the page origin.
function hasUnsafeProtocol(href: string | undefined): boolean {
  if (!href) return false;
  const m = /^([a-zA-Z][a-zA-Z0-9+.-]*):/.exec(href);
  if (!m) return false;
  const scheme = m[1].toLowerCase();
  return scheme !== "http" && scheme !== "https" && scheme !== "mailto" && scheme !== "tel";
}

function isExternalHref(href: string | undefined): boolean {
  if (!href) return false;
  // Bail on mailto:, tel:, # anchors, and protocol-less / relative paths —
  // those should keep their default in-place behaviour.
  if (!/^https?:\/\//i.test(href)) return false;
  if (typeof window === "undefined") {
    // Server render: assume any absolute http(s) link could be external.
    // Hydration on the client will re-evaluate against the real origin.
    return true;
  }
  try {
    const u = new URL(href);
    return u.host !== window.location.host;
  } catch {
    return false;
  }
}
