// lab の接続先・鍵・待ち合わせの道具（docs/lab.md）。
import { readFileSync } from 'node:fs';
import {
  CoingeckoSource,
  EsploraClient,
  EvmClient,
  FrankfurterSource,
  KeySet,
  MemoryStorage,
  PoolTransport,
  Session,
  UserClient,
  sleep,
  type Deployments,
  type RateSource,
  type UserOrder,
  type UserOrderStatus,
} from '@proxy-shopping/core/node';

export const NETWORK = 'ps-lab';
export const RELAYS = ['wss://relay-1.test', 'wss://relay-2.test'];
export const ESPLORA = 'https://esplora.test';
export const EVM_RPC = 'https://evm.test';
export const FAUCET = 'https://faucet.test';
export const RATES = 'https://rates.test';
export const ADMIN_TOKEN = 'lab';

export const ADDRESS = { name: '山田太郎', postal_code: '160-0022', address: '東京都新宿区新宿1-1-1', phone: '03-0000-0000' };

export type LabName =
  | 'coordinator-1' | 'coordinator-2' | 'operator-1' | 'operator-2' | 'shopper-1' | 'shopper-2'
  | 'escrow-1' | 'escrow-2' | 'escrow-nat' | 'user-1' | 'user-2' | 'user-browser' | 'relay-p2p' | 'faucet';

const keyCache = new Map<LabName, KeySet>();
export function keys(name: LabName): KeySet {
  let k = keyCache.get(name);
  if (!k) {
    k = KeySet.fromMnemonic(readFileSync(`/keys/${name}.mnemonic`, 'utf8').trim());
    keyCache.set(name, k);
  }
  return k;
}
export const pk = (name: LabName) => keys(name).nostrPublicKey;

/** 利用者のブラウザと同じ設定（lab/web-config.json を runner に焼き込んだもの） */
export const LAB_WEB_CONFIG = JSON.parse(readFileSync(new URL('../../lab/web-config.json', import.meta.url), 'utf8')) as {
  timelock_policy: Record<string, number>;
};

export function deployments(): Deployments {
  return JSON.parse(readFileSync('/deployments/31337.json', 'utf8')) as Deployments;
}

export function labRates(): RateSource[] {
  return [new CoingeckoSource(`${RATES}/coingecko`), new FrankfurterSource(`${RATES}/frankfurter`)];
}

/** A browser-less participant: the same core the web app runs, with in-memory storage. */
export function session(name: LabName, opts: { rates?: RateSource[]; coordinators?: string[] } = {}): Session {
  const k = keys(name);
  const d = deployments();
  return new Session({
    keys: k,
    transport: new PoolTransport(),
    storage: new MemoryStorage(),
    config: {
      network: NETWORK,
      relays: RELAYS,
      coordinators: opts.coordinators ?? [pk('coordinator-1'), pk('coordinator-2')],
      retryIntervalMs: 3000,
      timelockPolicy: LAB_WEB_CONFIG.timelock_policy,
    },
    chain: new EsploraClient(ESPLORA),
    evm: new EvmClient(d.chain_id, EVM_RPC, k.evmAccount, d),
    rates: opts.rates ?? labRates(),
  });
}

export async function startUser(name: LabName, opts: Parameters<typeof session>[1] = {}): Promise<{ s: Session; user: UserClient }> {
  const s = session(name, opts);
  await s.start();
  await s.publishInboxRelays();
  const user = new UserClient(s, { deployments: deployments() }).attach();
  // 捨てたメッセージとエラーは、失敗の原因を追えるように runner のログに出す
  if (process.env.E2E_TRACE) s.messenger.on('message', (m) => console.log(`[${name}] got ${m.type} ${m.orderId?.slice(0, 8)} from ${m.from.slice(0, 8)}`));
  s.messenger.on('dropped', ({ inner, reason }) => console.log(`[${name}] dropped ${inner.id.slice(0, 8)}: ${reason}`));
  user.on('error', ({ orderId, error }) => console.log(`[${name}] error ${orderId?.slice(0, 8) ?? ''}: ${error.message}`));
  return { s, user };
}

// ---- HTTP

async function request<T>(method: string, url: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
  const res = await fetch(url, {
    method,
    headers: { 'content-type': 'application/json', ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`${method} ${url}: ${res.status} ${text}`);
  return (text ? JSON.parse(text) : undefined) as T;
}

export const faucet = {
  btc: (address: string, sats: number) => request<{ txid: string }>('POST', `${FAUCET}/btc`, { address, sats }),
  evm: (address: string, usdc: string, eth = '10') => request('POST', `${FAUCET}/evm`, { address, eth, usdc }),
  mine: (blocks: number) => request<{ height: number }>('POST', `${FAUCET}/mine`, { blocks }),
  evmTime: (seconds: number) => request('POST', `${FAUCET}/evm/time`, { seconds }),
  height: () => request<{ btc: number; evm_time: number }>('GET', `${FAUCET}/height`),
};

/** psnode の管理 API（docs/lab.md）。 */
export function admin(host: string) {
  const base = `http://${host}:8080`;
  const auth = { authorization: `Bearer ${ADMIN_TOKEN}` };
  return {
    status: () => request<NodeStatus>('GET', `${base}/status`, undefined, auth),
    trust: () => request<unknown>('GET', `${base}/trust`, undefined, auth),
    events: (events: unknown[]) => request('POST', `${base}/events`, events, auth),
    p2pStatus: (peer_id: string) => request<unknown>('POST', `${base}/p2p/status`, { peer_id }, auth),
    order: (id: string) => request<ShopperOrder>('GET', `${base}/orders/${id}`, undefined, auth),
    cases: () => request<EscrowCase[]>('GET', `${base}/cases`, undefined, auth),
    case: (id: string) => request<EscrowCase>('GET', `${base}/cases/${id}`, undefined, auth),
    rule: (id: string, split: { user: string; shopper: string; reason: string }) => request<unknown>('POST', `${base}/cases/${id}/rule`, split, auth),
    reports: () => request<OperatorReport[]>('GET', `${base}/reports`, undefined, auth),
  };
}

export interface NodeStatus {
  pubkey: string;
  role: string;
  peer_id: string;
  addrs: string[];
  reachability: string;
  trust: Record<string, number>;
  peers?: string[];
}
export interface OperatorReport {
  id: string;
  from: string;
  invalid_evidence: number;
  report: { subject: string; order_id: string; text: string; evidence: unknown[] };
}
export interface ShopperOrder { id: string; state: string; error?: string; payout_tx?: string; payout_by?: string; ship_status?: string }
export interface EscrowCase {
  order_id: string;
  state: string;
  error?: string;
  delivery_address?: typeof ADDRESS;
  attachments?: Record<string, { mime: string; data_b64?: string }>;
}

/** Esplora で確定した tx の、指定アドレスへの出力の合計 */
export async function paidTo(txid: string, address: string): Promise<bigint> {
  const tx = await until(`tx ${txid.slice(0, 8)} confirmed`, 60_000, async () => {
    const t = (await (await fetch(`https://esplora.test/tx/${txid}`)).json()) as { status: { confirmed: boolean }; vout: Array<{ scriptpubkey_address?: string; value: number }> };
    return t.status.confirmed ? t : undefined;
  });
  return tx.vout.filter((o) => o.scriptpubkey_address === address).reduce((n, o) => n + BigInt(o.value), 0n);
}

// ---- 待ち合わせ

export async function until<T>(what: string, ms: number, probe: () => Promise<T | undefined | false>): Promise<T> {
  const end = Date.now() + ms;
  let last: unknown;
  for (;;) {
    try {
      const v = await probe();
      if (v) return v;
    } catch (e) {
      last = e;
    }
    if (Date.now() > end) throw new Error(`timeout: ${what}${last ? ` (last error: ${(last as Error).message ?? last})` : ''}`);
    await sleep(500);
  }
}

export async function waitOrder(user: UserClient, id: string, status: UserOrderStatus | UserOrderStatus[], ms = 90_000): Promise<UserOrder> {
  const want = Array.isArray(status) ? status : [status];
  let seen: UserOrder | undefined;
  try {
    return await until(`order ${id.slice(0, 8)} → ${want.join('|')}`, ms, async () => {
      seen = await user.getOrder(id);
      return seen && want.includes(seen.status) ? seen : undefined;
    });
  } catch (e) {
    throw new Error(`${(e as Error).message}; now ${seen?.status} ${seen?.lastError ?? ''}`);
  }
}

export async function waitShopper(host: string, id: string, states: string[], ms = 90_000): Promise<ShopperOrder> {
  return until(`${host} order ${id.slice(0, 8)} → ${states.join('|')}`, ms, async () => {
    const o = await admin(host).order(id);
    return states.includes(o.state) ? o : undefined;
  });
}

export function assert(cond: unknown, msg: string): asserts cond {
  if (!cond) throw new Error(`assertion failed: ${msg}`);
}
