// e2e の入口: docker compose run --rm runner [シナリオ id …]
// 結果は results/e2e-<時刻>.md と results/e2e-latest.md。1 つでも失敗すれば終了コード 1。
import { mkdirSync, writeFileSync } from 'node:fs';
import { faucet, keys, startUser } from './lab.js';
import { scenarios, type Ctx } from './scenarios/index.js';
import { depositBond, setupTrust, waitForLab } from './setup.js';

const wanted = process.argv.slice(2);
const selected = wanted.length ? scenarios.filter((s) => wanted.includes(s.id)) : scenarios;
if (!selected.length) {
  console.error(`unknown scenarios: ${wanted.join(' ')} (have ${scenarios.map((s) => s.id).join(' ')})`);
  process.exit(2);
}

const head: string[] = [];
const details: string[] = [];
const say = (l: string, to = head) => {
  console.log(l);
  to.push(l);
};

async function main(): Promise<number> {
  const started = new Date();
  say(`# proxy-shopping e2e (${started.toISOString()})`);
  await waitForLab();
  const trust = await setupTrust();
  say(`- trust: operator-1 list v${trust.op1}, operator-2 list v${trust.op2}`);
  await depositBond(100_000_000n);
  // shopper と escrow は Safe の取引を、operator は bond の没収を自分で実行するので、ガス代の ETH が要る
  for (const name of ['shopper-1', 'shopper-2', 'escrow-1', 'escrow-2', 'escrow-nat', 'operator-1'] as const) await faucet.evm(keys(name).evmAddress, '0');
  for (const name of ['user-1', 'user-2'] as const) {
    const k = keys(name);
    await faucet.btc(k.btcWallet.address, 2_000_000);
    await faucet.evm(k.evmAddress, '1000');
  }
  const u1 = await startUser('user-1');
  const u2 = await startUser('user-2');
  const ctx: Ctx = { users: { 'user-1': u1.user, 'user-2': u2.user }, log: (l) => say(`  - ${l}`, details) };
  const rows: string[] = [];
  let failed = 0;
  for (const s of selected) {
    const t0 = Date.now();
    say(`- **${s.id}**: ${s.title}`, details);
    try {
      await s.run(ctx);
      rows.push(`| ${s.id} | PASS | ${((Date.now() - t0) / 1000).toFixed(0)} | ${s.title} |`);
    } catch (e) {
      failed++;
      const msg = (e as Error).stack ?? String(e);
      say(`  - FAIL: ${msg.split('\n').slice(0, 6).join(' / ')}`, details);
      rows.push(`| ${s.id} | **FAIL** | ${((Date.now() - t0) / 1000).toFixed(0)} | ${s.title} |`);
    }
  }
  u1.s.stop();
  u2.s.stop();
  rows.forEach((r) => console.log(r));

  mkdirSync('results', { recursive: true });
  const md = [...head, '', '| id | result | seconds | scenario |', '|---|---|---:|---|', ...rows, '', '## 詳細', '', ...details].join('\n') + '\n';
  writeFileSync(`results/e2e-${started.toISOString().replace(/[:.]/g, '-')}.md`, md);
  writeFileSync('results/e2e-latest.md', md);
  console.log(failed ? `\n${failed} scenario(s) failed` : '\nall scenarios passed');
  return failed ? 1 : 0;
}

main().then(
  (code) => process.exit(code),
  (e) => {
    console.error(e);
    process.exit(1);
  },
);
