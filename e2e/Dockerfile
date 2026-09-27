# e2e の実行環境。compose の additional_contexts で proxy-shopping-web を "web" として受け取る。
FROM mcr.microsoft.com/playwright:v1.55.0-noble
RUN apt-get update && apt-get install -y --no-install-recommends iproute2 && rm -rf /var/lib/apt/lists/*

# 同じ相対位置（../../proxy-shopping-web）に置くので、file: 依存がそのまま解決できる
WORKDIR /src/proxy-shopping-web
COPY --from=web package.json package-lock.json tsconfig.base.json ./
COPY --from=web packages/core/package.json packages/core/
COPY --from=web apps/web/package.json apps/web/
RUN npm ci -w @proxy-shopping/core --include-workspace-root
COPY --from=web packages/core packages/core
RUN npm run build -w @proxy-shopping/core

WORKDIR /src/proxy-shopping-go/e2e
COPY package.json package-lock.json* ./
RUN npm install --no-audit --no-fund
COPY . .
ENTRYPOINT ["./entrypoint.sh"]
