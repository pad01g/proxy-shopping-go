// 信頼の初期設定: coordinator の委任書と operator の一覧を公開する（spec §2）。
// 毎回新しい版を出すので、何度実行しても同じ状態から始まる。
import { CoordinatorClient, OperatorClient, type ListEntry, type OperatorListContent } from '@proxy-shopping/core';
import { ESPLORA, EVM_RPC, admin, deployments, faucet, pk, session, until, type LabName } from './lab.js';

const PAYMENTS: ListEntry['payments'] = ['btc-signet', 'usdc-evm'];
const entry = (region: string, shopper: LabName, escrow: LabName): ListEntry => ({
  region, shopper: pk(shopper), escrow: pk(escrow), shops: ['*'], payments: PAYMENTS, tags: [], escrow_sla_days: 14,
});

/** operator-1（関東 + 米国）の一覧。escrow-2 はシナリオ c で外される */
export const operator1Entries = (): ListEntry[] => [
  entry('JP-13', 'shopper-1', 'escrow-1'),
  entry('JP-13', 'shopper-1', 'escrow-2'),
  entry('JP-13', 'shopper-1', 'escrow-nat'),
  entry('JP-13', 'shopper-2', 'escrow-1'),
  entry('US', 'shopper-1', 'escrow-1'),
];

/** operator-2（大阪）の一覧。シナリオ d で委任ごと失効させる */
export const operator2Entries = (): ListEntry[] => [entry('JP-27', 'shopper-2', 'escrow-1')];

function chainInfo(): OperatorListContent['chain'] {
  const d = deployments();
  return {
    btc: { network: 'signet', esplora: [ESPLORA] },
    evm: { chain_id: d.chain_id, rpc: [EVM_RPC], usdc: d.usdc, safe: { ...d.safe, module: d.module, setup: d.setup } },
  };
}

export async function publishList(name: 'operator-1' | 'operator-2', title: string, regions: string[], entries: ListEntry[]) {
  const s = session(name);
  const op = new OperatorClient(s);
  const list = await op.publish({ ...(await op.draft(title)), regions, entries, chain: chainInfo() });
  s.stop();
  return list;
}

export async function delegate(coordinator: 'coordinator-1' | 'coordinator-2', operator: LabName, revoke = false) {
  const s = session(coordinator);
  const c = new CoordinatorClient(s);
  const d = revoke ? await c.revoke(pk(operator), 'e2e') : await c.delegate(pk(operator), 'e2e');
  s.stop();
  return d;
}

const NODES = ['shopper-1', 'shopper-2', 'escrow-1', 'escrow-2', 'escrow-nat', 'operator-1', 'operator-2', 'p2p-relay'] as const;

export async function waitForLab(): Promise<void> {
  await until('faucet ready', 240_000, async () => (await faucet.height()).btc > 100);
  await until('esplora ready', 60_000, async () => (await fetch(`${ESPLORA}/blocks/tip/height`)).ok);
  for (const n of NODES) await until(`${n} admin`, 60_000, async () => !!(await admin(n).status()).pubkey);
}

/** 各ノードが operator の一覧を、指定した版以上で持つまで待つ */
export async function waitTrust(nodes: readonly string[], operator: LabName, version: number, ms = 60_000): Promise<void> {
  for (const n of nodes) {
    await until(`${n} holds ${operator} list v${version}`, ms, async () => ((await admin(n).status()).trust[pk(operator)] ?? 0) >= version);
  }
}

export async function setupTrust(): Promise<{ op1: number; op2: number }> {
  await delegate('coordinator-1', 'operator-1');
  await delegate('coordinator-2', 'operator-2');
  const l1 = await publishList('operator-1', 'Kanto + US operator', ['JP-13', 'US'], operator1Entries());
  const l2 = await publishList('operator-2', 'Kansai operator', ['JP-27'], operator2Entries());
  await waitTrust(NODES, 'operator-1', l1.version);
  await waitTrust(NODES, 'operator-2', l2.version);
  return { op1: l1.version, op2: l2.version };
}

/** escrow-2 は operator-1 との規約で bond を預けている（contracts/examples/bond） */
export async function depositBond(amount: bigint): Promise<void> {
  const s = session('escrow-2');
  await faucet.evm(s.keys.evmAddress, (amount / 1_000_000n).toString());
  await s.evm!.bondDeposit(amount);
}
