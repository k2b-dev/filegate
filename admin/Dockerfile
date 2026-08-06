FROM oven/bun:1.3.14 AS deps
WORKDIR /app
COPY sdk/ts ./sdk/ts
COPY admin ./admin
WORKDIR /app/admin
RUN bun install --frozen-lockfile

FROM deps AS build
WORKDIR /app/admin
RUN bun run build

FROM oven/bun:1.3.14-slim
WORKDIR /app/admin
ENV PORT=3000
COPY --from=build /app/admin/package.json ./package.json
COPY --from=build /app/admin/node_modules ./node_modules
COPY --from=build /app/admin/dist ./dist
COPY --from=build /app/sdk/ts /app/sdk/ts
EXPOSE 3000
CMD ["bun", "dist/server.js"]
