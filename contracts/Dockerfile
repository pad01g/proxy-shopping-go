# deployer: 起動すると RPC_URL へコントラクトを置き、OUT へアドレスの JSON を書く。
# solc の取得とコンパイルはイメージを作るときに済ませるので、実行時にネットワークは要らない。
FROM ghcr.io/foundry-rs/foundry:stable

USER root
RUN mkdir -p /contracts /deployments && chown foundry:foundry /contracts /deployments
USER foundry
WORKDIR /contracts

COPY --chown=foundry:foundry foundry.toml deploy.sh ./
COPY --chown=foundry:foundry lib ./lib
COPY --chown=foundry:foundry src ./src
COPY --chown=foundry:foundry examples ./examples
COPY --chown=foundry:foundry script ./script
RUN mkdir -p deployments && forge build

ENV FOUNDRY_OFFLINE=true \
    RPC_URL=http://anvil:8545 \
    OUT=/deployments/31337.json

ENTRYPOINT ["/contracts/deploy.sh"]
