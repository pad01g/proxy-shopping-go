# Build context: shopper-bot/. The playwright npm version is pinned to the
# image version (1.55.0) so the bundled Chromium matches.
FROM mcr.microsoft.com/playwright:v1.55.0-noble AS build
WORKDIR /app
ENV PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY tsconfig.json tsconfig.build.json ./
COPY src ./src
RUN npm run build && npm prune --omit=dev

FROM mcr.microsoft.com/playwright:v1.55.0-noble
WORKDIR /app
ENV NODE_ENV=production PORT=7000 BOT_DATA_DIR=/data
# purchase results per request_id (spec §9); a fresh named volume takes this owner
RUN mkdir -p /data && chown pwuser:pwuser /data
COPY --from=build /app/node_modules ./node_modules
COPY --from=build /app/dist ./dist
COPY package.json ./
COPY schema ./schema
COPY lab ./lab
USER pwuser
EXPOSE 7000
CMD ["node", "dist/main.js"]
