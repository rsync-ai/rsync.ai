FROM node:26-alpine AS deps
WORKDIR /app

# Copy package files
COPY package.json package-lock.json* ./

# Cache npm packages across builds — avoids re-downloading on every build
RUN --mount=type=cache,target=/root/.npm \
    npm ci

FROM node:26-alpine AS builder
WORKDIR /app

# Copy dependencies
COPY --from=deps /app/node_modules ./node_modules
COPY . .

# Set environment variables for build
ENV NEXT_TELEMETRY_DISABLED=1
ENV NODE_ENV=production

# NEXT_PUBLIC_* vars are baked into the client bundle at build time.
# Pass the production values via --build-arg in CI; defaults allow local builds.
ARG NEXT_PUBLIC_API_URL=http://localhost:5001
ARG NEXT_PUBLIC_WS_URL=ws://localhost:5001/ws
ENV NEXT_PUBLIC_API_URL=$NEXT_PUBLIC_API_URL
ENV NEXT_PUBLIC_WS_URL=$NEXT_PUBLIC_WS_URL
ARG NEXT_PUBLIC_FEATURE_MONITORING_OVERVIEW=false
ENV NEXT_PUBLIC_FEATURE_MONITORING_OVERVIEW=$NEXT_PUBLIC_FEATURE_MONITORING_OVERVIEW
ARG NEXT_PUBLIC_FEATURE_MONITORING_INFRA=false
ENV NEXT_PUBLIC_FEATURE_MONITORING_INFRA=$NEXT_PUBLIC_FEATURE_MONITORING_INFRA
ARG NEXT_PUBLIC_FEATURE_MONITORING_TRACES=false
ENV NEXT_PUBLIC_FEATURE_MONITORING_TRACES=$NEXT_PUBLIC_FEATURE_MONITORING_TRACES
ARG NEXT_PUBLIC_OTEL_ENDPOINT=
ENV NEXT_PUBLIC_OTEL_ENDPOINT=$NEXT_PUBLIC_OTEL_ENDPOINT
ARG NEXT_PUBLIC_OTEL_SERVICE_NAME=frontend
ENV NEXT_PUBLIC_OTEL_SERVICE_NAME=$NEXT_PUBLIC_OTEL_SERVICE_NAME

# Cache .next/cache across builds — Next.js incremental compilation
# reuses compiled pages/modules from previous runs (saves 3-4 min on rebuilds)
RUN --mount=type=cache,target=/app/.next/cache \
    npm run build

FROM node:26-alpine AS runner
WORKDIR /app

ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1

# Create non-root user
RUN addgroup --system --gid 1001 nodejs
RUN adduser --system --uid 1001 nextjs

# Copy necessary files
COPY --from=builder /app/public ./public
COPY --from=builder --chown=nextjs:nodejs /app/.next/standalone ./
COPY --from=builder --chown=nextjs:nodejs /app/.next/static ./.next/static

# The licence attributions for what this image ships. A repo-root file this build's
# context cannot reach, so it comes through the `legal` named context (the repo root).
COPY --from=legal THIRD_PARTY_NOTICES.md /licenses/THIRD_PARTY_NOTICES.md

USER nextjs

EXPOSE 3000

ENV PORT=3000
ENV HOSTNAME="0.0.0.0"

CMD ["node", "server.js"]
