import { createBot } from "./bot.js";
import { loadConfig } from "./config.js";

const config = loadConfig();
const bot = createBot(config);
bot.server.listen(config.port, config.host, () => {
  console.log(`[bot] listening on ${config.host}:${config.port}`);
});

for (const signal of ["SIGINT", "SIGTERM"] as const) {
  process.on(signal, () => {
    void bot.close().finally(() => process.exit(0));
  });
}
