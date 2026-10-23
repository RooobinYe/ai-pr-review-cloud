FROM node:24-slim AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts
COPY tsconfig.json ./
COPY src ./src
RUN npm run build && npm prune --omit=dev

FROM registry.k8s.io/kubectl:v1.34.1 AS kubectl

FROM node:24-slim
# git backs the git tool; pi's grep and find tools use ripgrep and fd, and would
# otherwise try to download them at runtime, which the runner's network policy blocks.
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates ripgrep fd-find \
    && rm -rf /var/lib/apt/lists/*
COPY --from=kubectl /bin/kubectl /usr/local/bin/kubectl
WORKDIR /app
COPY --from=build /app/node_modules ./node_modules
COPY --from=build /app/dist ./dist
COPY package.json ./
USER node
ENTRYPOINT ["node", "/app/dist/main.js"]
