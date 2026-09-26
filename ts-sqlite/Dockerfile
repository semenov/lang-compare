# Build context: repo root (needs infra/sqlite.sql).
FROM node:26-alpine AS build
WORKDIR /app
COPY ts-sqlite/package.json ts-sqlite/package-lock.json ./
RUN npm ci
COPY ts-sqlite/tsconfig.json ./
COPY ts-sqlite/src ./src
RUN npx tsc -p . && npm prune --omit=dev

FROM node:26-alpine
WORKDIR /app
ENV NODE_ENV=production SQLITE_PATH=/data/app.db SQLITE_INIT=/app/sqlite.sql
COPY --from=build /app/node_modules ./node_modules
COPY --from=build /app/dist ./dist
COPY ts-sqlite/package.json ./
COPY infra/sqlite.sql ./
USER node
CMD ["node", "dist/main.js"]
