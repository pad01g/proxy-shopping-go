import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { BotService, Reply } from "./service.js";

const maxBody = 1 << 20;

export function createHttpServer(service: BotService): Server {
  return createServer((req, res) => {
    route(service, req)
      .then((reply) => send(res, reply))
      .catch((err: Error) => send(res, { status: 500, body: { error: err.message } }));
  });
}

async function route(service: BotService, req: IncomingMessage): Promise<Reply> {
  const path = new URL(req.url ?? "/", "http://bot").pathname;
  const key = `${req.method} ${path}`;
  switch (key) {
    case "GET /healthz":
      return { status: 200, body: { ok: true } };
    case "GET /v1/capabilities":
      return service.capabilities();
    case "POST /v1/purchase":
    case "POST /v1/tracking": {
      let body: unknown;
      try {
        body = JSON.parse(await readBody(req));
      } catch (err) {
        return { status: 400, body: { error: `body must be JSON: ${(err as Error).message}` } };
      }
      return path === "/v1/purchase" ? service.purchase(body) : service.tracking(body);
    }
    default:
      return { status: 404, body: { error: `no route ${key}` } };
  }
}

function readBody(req: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    req.on("data", (c: Buffer) => {
      size += c.length;
      if (size > maxBody) {
        reject(new Error("request body too large"));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
    req.on("error", reject);
  });
}

function send(res: ServerResponse, reply: Reply): void {
  const body = JSON.stringify(reply.body);
  res.writeHead(reply.status, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(body) });
  res.end(body);
}
