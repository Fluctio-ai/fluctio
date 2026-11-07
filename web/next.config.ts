import type { NextConfig } from "next";

// Static export is a BUILD-time requirement (the gateway serves the flat
// web/out via its embedded FS). It must NOT apply to `next dev`: export
// mode forces generateStaticParams validation on every dynamic route, and
// chat pages are keyed by runtime agent/session ids that can't be
// enumerated — which made /agents/[id]/chat 500 in dev. Set
// STATIC_EXPORT=1 for the release/dev-build pipeline; plain `next dev`
// runs without it and behaves like a normal dynamic app.
const staticExport = process.env.STATIC_EXPORT === "1";

const nextConfig: NextConfig = {
  ...(staticExport ? { output: "export" as const } : {}),
  trailingSlash: true,
  images: {
    unoptimized: true,
  },
  // Dev-only API proxy: `next dev` serves dynamically, so rewrites work
  // here even though they're skipped by the static export build. Point
  // /api and /v1 at the local gateway so HMR-driven UI testing hits real
  // backend data without a reverse proxy in front. Override the target
  // with FLUCTIO_GATEWAY_ORIGIN if the gateway runs elsewhere.
  async rewrites() {
    const gateway = process.env.FLUCTIO_GATEWAY_ORIGIN ?? "http://127.0.0.1:18953";
    return [
      { source: "/api/:path*", destination: `${gateway}/api/:path*` },
      { source: "/v1/:path*", destination: `${gateway}/v1/:path*` },
    ];
  },
};

export default nextConfig;
