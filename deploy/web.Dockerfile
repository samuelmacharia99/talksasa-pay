# syntax=docker/dockerfile:1.7
# Builds one Next.js app from the web/ workspace (build context: ./web).
#   docker build -f deploy/web.Dockerfile --build-arg APP=admin web
ARG NODE_IMAGE=node:24.11-alpine3.22

FROM ${NODE_IMAGE} AS deps
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0 NEXT_TELEMETRY_DISABLED=1
RUN corepack enable
WORKDIR /web
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml .npmrc tsconfig.base.json ./
COPY apps/admin/package.json apps/admin/
COPY apps/pay/package.json apps/pay/
COPY packages/api-client/package.json packages/api-client/
COPY packages/money/package.json packages/money/
COPY packages/ui/package.json packages/ui/
RUN pnpm install --frozen-lockfile

FROM deps AS build
ARG APP
COPY . .
RUN test -n "$APP" && pnpm --filter "@talksasa/${APP}" build

FROM ${NODE_IMAGE} AS runtime
ARG APP
ENV NODE_ENV=production NEXT_TELEMETRY_DISABLED=1 HOSTNAME=0.0.0.0
WORKDIR /app
# Standalone output: only the traced server files, no dev dependencies.
COPY --from=build --chown=node:node /web/apps/${APP}/.next/standalone ./
COPY --from=build --chown=node:node /web/apps/${APP}/.next/static ./apps/${APP}/.next/static
COPY --from=build --chown=node:node /web/apps/${APP}/public ./apps/${APP}/public
WORKDIR /app/apps/${APP}
USER node
CMD ["node", "server.js"]
