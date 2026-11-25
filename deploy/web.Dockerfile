# syntax=docker/dockerfile:1.7
# Builds one Next.js app from the pnpm workspace (build context: the
# repository root; the workspace root is the repository root, the code is
# under web/). deploy/web.Dockerfile.dockerignore limits the context.
#   docker build -f deploy/web.Dockerfile --build-arg APP=admin .
ARG NODE_IMAGE=node:24.11-alpine3.22

FROM ${NODE_IMAGE} AS deps
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0 NEXT_TELEMETRY_DISABLED=1
RUN corepack enable
WORKDIR /repo
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml .npmrc ./
COPY web/tsconfig.base.json web/
COPY web/apps/admin/package.json web/apps/admin/
COPY web/apps/pay/package.json web/apps/pay/
COPY web/packages/api-client/package.json web/packages/api-client/
COPY web/packages/money/package.json web/packages/money/
COPY web/packages/ui/package.json web/packages/ui/
RUN pnpm install --frozen-lockfile

FROM deps AS build
ARG APP
COPY web web
RUN test -n "$APP" && pnpm --filter "@talksasa/${APP}" build

FROM ${NODE_IMAGE} AS runtime
ARG APP
ENV NODE_ENV=production NEXT_TELEMETRY_DISABLED=1 HOSTNAME=0.0.0.0
WORKDIR /app
# Standalone output (traced from the workspace root): only the server
# files, no dev dependencies.
COPY --from=build --chown=node:node /repo/web/apps/${APP}/.next/standalone ./
COPY --from=build --chown=node:node /repo/web/apps/${APP}/.next/static ./web/apps/${APP}/.next/static
COPY --from=build --chown=node:node /repo/web/apps/${APP}/public ./web/apps/${APP}/public
WORKDIR /app/web/apps/${APP}
USER node
CMD ["node", "server.js"]
