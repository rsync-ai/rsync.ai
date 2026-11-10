import type { NextConfig } from "next";
import { withSentryConfig } from "@sentry/nextjs/config";

const nextConfig: NextConfig = {
  output: 'standalone',
  reactStrictMode: true,
  // The dashboard PDF export (src/app/proxy/dashboards/[id]/pdf): pdfkit reads its
  // own data files from beside its code, so it runs from node_modules, not a bundle;
  // and the Inter it embeds is read from src/assets/fonts at run time, which the
  // standalone server does not carry unless it is named here. A workflow chart's PNG
  // (src/app/api/internal/charts/png) measures with pdfkit and draws with resvg, a
  // native module that loads its platform's binary at run time, from the same fonts.
  serverExternalPackages: ['pdfkit', 'svg-to-pdfkit', '@resvg/resvg-js'],
  outputFileTracingIncludes: {
    '/proxy/dashboards/*/pdf': ['./src/assets/fonts/**/*'],
    '/api/internal/charts/png': ['./src/assets/fonts/**/*'],
  },
  // A bare `echarts` import is the whole library, every chart and component registered.
  // Our code imports from `echarts/core` and registers what it draws; the custom-series
  // packages (dashboards-design.md § 14.6) import `echarts` itself: one such import, built
  // without this alias, took the chart chunks from 190 KB to 396 KB gzipped. The key
  // matches `echarts` alone, not `echarts/charts` or the other subpaths (with it, today's
  // chunk builds byte for byte the same). scripts/check-chart-bundle-size.mjs holds the
  // chart chunks to a budget.
  turbopack: {
    resolveAlias: { echarts: 'echarts/core' },
  },
  async redirects() {
    return [
      // `/home` has no page; the sidebar "Home" link uses `/`. Redirect direct
      // navigation / bookmarks to the dashboard root instead of a 404.
      { source: '/home', destination: '/', permanent: false },
      // Scheduled Queries became Models, and the page moved with the name. Links
      // already out there (bookmarks, an alert email, a pasted URL) still land.
      { source: '/explorer/schedules', destination: '/models', permanent: false },
      { source: '/explorer/schedules/:id', destination: '/models/:id', permanent: false },
    ]
  },
};

export default withSentryConfig(nextConfig, {
  // Suppress Sentry CLI output except on errors
  silent: !process.env.CI,

  // Only upload source maps in CI / production builds
  widenClientFileUpload: true,
  sourcemaps: {
    deleteSourcemapsAfterUpload: true,
  },
  webpack: {
    treeshake: { removeDebugLogging: true },
  },

  // Avoid build failure when SENTRY_DSN is missing in dev
  telemetry: false,
});
