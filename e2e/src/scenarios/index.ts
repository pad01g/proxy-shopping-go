// e2e のシナリオ（docs/lab.md）。a〜f はユーザーと決めたもの、g・h はタイムロックの確認。
import { StaticSource, signInner, type Session, type UserClient } from '@proxy-shopping/core';
import { ADDRESS, admin, assert, faucet, keys, paidTo, pk, startUser, until, waitOrder, waitShopper, type LabName } from '../lab.js';
import { acceptAndFund, placeFunded, requestQuote } from '../orders.js';
import { delegate, operator1Entries, publishList, waitTrust } from '../setup.js';
import { browserHappyPath } from './browser.js';

export interface Ctx {
  users: Record<'user-1' | 'user-2', UserClient>;
  sessions: Record<'user-1' | 'user-2', Session>;
  log: (line: string) => void;
}

export type Scenario = { id: string; title: string; run: (ctx: Ctx) => Promise<void> };

const SAFE_SHOP = 'https://safe-shop.test/';
const SHINJUKU = 'JP-13-13104';

async function usdcOf(name: LabName): Promise<bigint> {
  return (await startUser(name)).s.evm!.usdcBalance();
}

export const scenarios: Scenario[] = [
  {
    id: 'a',
    title: '正常系: BTC（safe-shop, JPY）と USDC（us-shop, USD）。見積のレート検証、escrow の前払い手数料、shopper への支払い',
    async run({ users, log }) {
      const user = users['user-1'];
      // BTC
      const btc = await placeFunded(user, { shopUrl: SAFE_SHOP, region: SHINJUKU, sku: 'A-100', payment: 'btc-signet', shopper: 'shopper-1', escrow: 'escrow-1' });
      assert(btc.quoteCheck?.fx?.level === 'ok', `fx level ${btc.quoteCheck?.fx?.level}`);
      await user.release(btc.id);
      const done = await waitOrder(user, btc.id, 'completed');
      const so = await waitShopper('shopper-1', btc.id, ['completed']);
      assert(so.payout_by === 'release', `payout_by ${so.payout_by}`);
      const st = await until('BTC payout confirmed', 30_000, async () => (await fetch(`https://esplora.test/tx/${done.completedTxid}/status`).then((r) => r.json())).confirmed);
      log(`BTC order ${btc.id.slice(0, 8)}: lock ${btc.quote!.lock_amount} sats, payout ${done.completedTxid} confirmed=${st}`);

      // USDC（米国の店、USD 建て）
      const shopperBefore = await usdcOf('shopper-1');
      const escrowBefore = await usdcOf('escrow-1');
      const usd = await placeFunded(user, { shopUrl: 'https://us-shop.test/', region: 'US', sku: 'U-100', payment: 'usdc-evm', shopper: 'shopper-1', escrow: 'escrow-1' });
      assert(usd.quote!.fx!.pair === 'USDC/USD', `pair ${usd.quote!.fx!.pair}`);
      await user.release(usd.id);
      await waitOrder(user, usd.id, 'completed');
      await waitShopper('shopper-1', usd.id, ['completed']);
      const got = (await usdcOf('shopper-1')) - shopperBefore;
      const fee = (await usdcOf('escrow-1')) - escrowBefore;
      assert(got === BigInt(usd.quote!.lock_amount!), `shopper got ${got}, lock ${usd.quote!.lock_amount}`);
      assert(fee === BigInt(usd.quote!.escrow_upfront_fee!), `escrow upfront fee ${fee} vs ${usd.quote!.escrow_upfront_fee}`);
      log(`USDC order ${usd.id.slice(0, 8)}: shopper +${got}, escrow upfront +${fee}`);

      // レートが大きくずれた見積には強い警告が出る（利用者側の取得元が 12% 安い BTC/USD を返す）
      const skewed = await startUser('user-2', { rates: [new StaticSource({ 'BTC/USD': 88000, 'USD/JPY': 150, 'USDC/USD': 1 })] });
      const q = await requestQuote(skewed.user, { shopUrl: SAFE_SHOP, region: SHINJUKU, sku: 'A-100', payment: 'btc-signet', shopper: 'shopper-1', escrow: 'escrow-1' });
      assert(q.quoteCheck?.fx?.level === 'strong', `expected strong warning, got ${q.quoteCheck?.fx?.level} (${q.quoteCheck?.fx?.deviation})`);
      await skewed.user.cancel(q.id);
      skewed.s.stop();
      log(`rate check: deviation ${q.quoteCheck?.fx?.deviation} → ${q.quoteCheck?.fx?.level}`);
    },
  },
  {
    id: 'b',
    title: '配達失敗 → 紛争 → 誠実な escrow-1 が返金を裁定 → 利用者が連署（BTC）。偽の利用者による紛争の乗っ取りは効かない',
    async run({ users, log }) {
      const user = users['user-1'];
      const o = await placeFunded(user, { shopUrl: SAFE_SHOP, region: SHINJUKU, sku: 'FAIL-100', payment: 'btc-signet', shopper: 'shopper-1', escrow: 'escrow-1' }, 'delivery_failed');
      // 攻撃: 捨て鍵の「偽の利用者」が、本物の利用者の公開鍵を写し、返金先だけ自分にした request で先に紛争を開く
      const mallory = await startUser('faucet');
      const forged = await signInner(mallory.s.signer, {
        recipient: pk('escrow-1'), orderId: o.id, type: 'order.request',
        body: { ...o.request, user_btc_address: mallory.s.keys.btcWallet.address },
      });
      await mallory.s.messenger.send(pk('escrow-1'), o.id, 'dispute.open', {
        claim: 'not_delivered', text: '返金して', evidence: { messages: [forged], tracking: [], purchase_evidence: [] },
      });
      await new Promise((r) => setTimeout(r, 4000));
      mallory.s.stop();
      await user.openDispute(o.id, { claim: 'not_delivered', text: '配送に失敗したと通知が来ました' });
      const c = await until('escrow-1 case collects evidence', 60_000, async () => {
        const c = await admin('escrow-1').case(o.id);
        return c.state === 'open' && c.delivery_address ? c : undefined;
      });
      assert(c.delivery_address?.address === ADDRESS.address, 'escrow decrypted the delivery address from key_for_escrow');
      assert((c as { user?: string }).user === pk('user-1'), `case user is ${(c as { user?: string }).user}, not the forger`);
      // 購入画面のスクリーンショットは 1 通に収まらないので、添付として分けて届く（spec §4.9）
      const shot = await until('escrow-1 assembled the screenshot', 60_000, async () =>
        Object.values((await admin('escrow-1').case(o.id)).attachments ?? {}).find((a) => a.mime === 'image/png' && a.data_b64));
      log(`screenshot attachment assembled: ${Math.round((shot.data_b64!.length * 3) / 4 / 1024)} KiB`);
      const lock = BigInt(o.quote!.lock_amount!) - BigInt(o.quote!.payout_fee_reserve ?? '0');
      // 紛争手数料は 2%。BTC ではダスト（546 sats 未満）になるなら 0（§4.8）
      const pct = (lock * 200n) / 10_000n;
      const fee = pct < 546n ? 0n : pct;
      await admin('escrow-1').rule(o.id, { user: String(lock - fee), shopper: '0', reason: '配送失敗のため全額返金' });
      await waitOrder(user, o.id, 'ruled');
      const problems = await user.reviewRuling(o.id);
      assert(problems.length === 0, `ruling review: ${problems.join('; ')}`);
      const settled = await user.countersignRuling(o.id);
      const refunded = await paidTo(settled.settledTxid!, o.request.user_btc_address!);
      assert(refunded === lock - fee, `refund output ${refunded}, want ${lock - fee}`);
      await waitShopper('shopper-1', o.id, ['settled', 'closed']);
      log(`dispute ${o.id.slice(0, 8)}: refunded ${lock - fee} sats to user (escrow fee ${fee}), tx ${settled.settledTxid}`);
    },
  },
  {
    id: 'c',
    title: 'escrow-2 の不正な裁定 → 通報 → operator-1 が一覧から外し、規約の bond を没収して補償（USDC）',
    async run({ users, log }) {
      const user = users['user-1'];
      const o = await placeFunded(user, { shopUrl: SAFE_SHOP, region: SHINJUKU, sku: 'FAIL-100', payment: 'usdc-evm', shopper: 'shopper-1', escrow: 'escrow-2' }, 'delivery_failed');
      await user.openDispute(o.id, { claim: 'not_delivered', text: '届いていません' });
      await until('escrow-2 case open', 60_000, async () => (await admin('escrow-2').case(o.id)).state === 'open');
      // 配送に失敗したのに、全額を shopper に配分する
      const lock = BigInt(o.quote!.lock_amount!);
      const fee = (lock * 200n) / 10_000n;
      await admin('escrow-2').rule(o.id, { user: '0', shopper: String(lock - fee), reason: '（不正）shopper に全額' });
      // shopper-1 は accept_rulings: always なので連署して執行する
      await waitShopper('shopper-1', o.id, ['settled']);
      await waitOrder(user, o.id, ['ruled', 'settled']);
      const problems = await user.reviewRuling(o.id);
      log(`user sees the ruling as: ${problems.join('; ') || '(no automatic objection)'}`);

      await user.report(o.id, { subject: 'escrow', text: '配送失敗なのに全額を shopper に配分した' });
      const report = await until('operator-1 received the report', 60_000, async () =>
        (await admin('operator-1').reports()).find((r) => r.report.order_id === o.id));
      assert(report.from === pk('user-1') && report.report.subject === pk('escrow-2'), 'report from user-1 about escrow-2');
      assert(report.invalid_evidence === 0, `invalid evidence ${report.invalid_evidence}`);
      assert(report.report.evidence.length >= 4, `evidence messages ${report.report.evidence.length}`);

      // operator-1: 一覧の新しい版から escrow-2 を外す
      const v = await publishList('operator-1', 'Kanto + US operator', ['JP-13', 'US'], operator1Entries().filter((e) => e.escrow !== pk('escrow-2')));
      await waitTrust(['shopper-1', 'escrow-nat', 'operator-1'], 'operator-1', v.version);
      const offers = await user.discoverOffers({ shopUrl: SAFE_SHOP, region: SHINJUKU, payment: 'usdc-evm', refresh: true });
      assert(!offers.some((x) => x.entry.escrow === pk('escrow-2')), 'escrow-2 is no longer offered');

      // 規約（プロトコルの外）: bond を没収して利用者に補償する
      const op = (await startUser('operator-1')).s;
      const before = await usdcOf('user-1');
      await op.evm!.bondSlash(keys('escrow-2').evmAddress, keys('user-1').evmAddress, lock);
      const comp = (await usdcOf('user-1')) - before;
      assert(comp === lock, `compensation ${comp}`);
      op.stop();
      log(`fraud ${o.id.slice(0, 8)}: reported, list v${v.version} without escrow-2, bond slashed ${comp} to user`);
    },
  },
  {
    id: 'd',
    title: 'coordinator-2 が operator-2 の委任を失効 → その一覧の組み合わせが候補から消える',
    async run({ users, log }) {
      const user = users['user-2'];
      const osaka = { shopUrl: SAFE_SHOP, region: 'JP-27-27100', payment: 'btc-signet' as const };
      const before = await user.discoverOffers({ ...osaka, refresh: true });
      assert(before.some((x) => x.entry.provenance.operator === pk('operator-2')), `operator-2 offers before revoke: ${before.length}`);
      const d = await delegate('coordinator-2', 'operator-2', true);
      await until('offers from operator-2 disappear', 60_000, async () =>
        !(await user.discoverOffers({ ...osaka, refresh: true })).some((x) => x.entry.provenance.operator === pk('operator-2')));
      // shopper-2 のノードも同じ判断をする: その組み合わせでの注文は trust で断る
      await until('shopper-2 dropped operator-2', 60_000, async () => {
        const t = (await admin('shopper-2').trust()) as { effective?: Array<{ operator: string }> };
        return !(t.effective ?? []).some((e) => e.operator === pk('operator-2'));
      });
      await delegate('coordinator-2', 'operator-2'); // 次の実行のために戻す
      log(`revoked operator-2 (delegation v${d.version}); offers for JP-27 went from ${before.length} to 0 and came back after re-delegation`);
    },
  },
  {
    id: 'e',
    title: 'NAT: NAT の内側のブラウザが公開の Web 画面から注文する（1 対 1 のメッセージは Nostr）。NAT の内側の escrow ノードへは libp2p の circuit relay で問い合わせが届く',
    async run({ log }) {
      const nat = await admin('escrow-nat').status();
      assert(nat.reachability === 'private', `escrow-nat reachability ${nat.reachability}`);
      assert(nat.addrs.some((a) => a.includes('/p2p-circuit')), 'escrow-nat has a relay address');
      assert(!nat.addrs.some((a) => a.startsWith('/ip4/172.40.') && !a.includes('/p2p-circuit')), 'escrow-nat has no direct public address');
      const st = (await admin('shopper-1').p2pStatus(nat.peer_id)) as { status?: { pubkey?: string }; limited_connection?: boolean };
      assert(st.status?.pubkey === pk('escrow-nat'), `status over libp2p: ${JSON.stringify(st).slice(0, 200)}`);
      // 公開側から NAT の内側へは経路が無いので、つながった以上 relay 経由か、DCUtR の穴あけに成功したかのどちらか
      log(`libp2p /ps/status from shopper-1 to escrow-nat: ok (${st.limited_connection ? 'over the circuit relay' : 'direct after DCUtR hole punching'})`);
      // 利用者のブラウザも NAT の内側（この runner 自体が router の内側にいる）
      const r = await browserHappyPath();
      log(`browser (behind NAT) order ${r.orderId.slice(0, 8)} completed, payout ${r.txid}`);
    },
  },
  {
    id: 'f',
    title: 'shopper の方針: 危険な店と地域外の現金店を断り、地域内の shopper は現金店の注文を受けて届ける',
    async run({ users, log }) {
      const user = users['user-2'];
      const risky = await requestQuote(user, { shopUrl: 'http://risky-shop.test/', region: 'US', sku: 'R-100', payment: 'usdc-evm', shopper: 'shopper-1', escrow: 'escrow-1' });
      assert(risky.status === 'rejected' && risky.quote?.reject_reason === 'risk', `risky: ${risky.status} ${risky.quote?.reject_reason}`);
      const cash = { shopUrl: 'https://cash-store.test/', region: SHINJUKU, sku: 'C-100', payment: 'btc-signet' as const, escrow: 'escrow-1' as const };
      const far = await requestQuote(user, { ...cash, shopper: 'shopper-1' });
      assert(far.status === 'rejected' && far.quote?.reject_reason === 'region', `out of region: ${far.status} ${far.quote?.reject_reason}`);
      const near = await acceptAndFund(user, await requestQuote(user, { ...cash, shopper: 'shopper-2' }));
      await user.release(near.id);
      await waitOrder(user, near.id, 'completed');
      const so = await waitShopper('shopper-2', near.id, ['completed']);
      log(`risky → ${risky.quote?.reject_reason}; cash-store via shopper-1 (JP-27) → ${far.quote?.reject_reason}; via shopper-2 (JP-13) → ${so.state}`);
    },
  },
  {
    id: 'g',
    title: 'タイムロック T1: 利用者が受け取り確認をしないまま T1 を過ぎると、shopper が単独で受け取る（BTC）。偽の「連署しました」では止まらない',
    async run({ users, sessions, log }) {
      const user = users['user-2'];
      const o = await placeFunded(user, { shopUrl: SAFE_SHOP, region: SHINJUKU, sku: 'A-100', payment: 'btc-signet', shopper: 'shopper-1', escrow: 'escrow-1' });
      // 攻撃: 受け取った利用者が「連署・放送した」と嘘をつき、shopper に T1 の受け取りをやめさせようとする
      await sessions['user-2'].messenger.send(pk('shopper-1'), o.id, 'dispute.countersigned', { txid: '00'.repeat(32) });
      await new Promise((r) => setTimeout(r, 5000));
      const still = await admin('shopper-1').order(o.id);
      assert(still.state === 'delivered', `a bogus countersigned moved the shopper to ${still.state}`);
      const t1 = o.quote!.timelock!.t1;
      const { btc } = await faucet.height();
      await faucet.mine(t1 - btc + 1);
      const so = await waitShopper('shopper-1', o.id, ['claimed'], 120_000);
      const got = await paidTo(so.payout_tx!, o.quote!.shopper_btc_address!);
      const want = BigInt(o.quote!.lock_amount!) - BigInt(o.quote!.payout_fee_reserve ?? '0');
      assert(got === want, `T1 claim paid ${got} to the shopper, want ${want}`);
      log(`T1=${t1}: shopper-1 claimed ${got} sats alone, tx ${so.payout_tx}`);
    },
  },
  {
    id: 'h',
    title: 'タイムロック T2: shopper が消えたら、T2 を過ぎた後に利用者が単独で取り戻す（USDC）',
    async run({ users, log }) {
      const user = users['user-2'];
      const o = await requestQuote(user, { shopUrl: SAFE_SHOP, region: SHINJUKU, sku: 'A-200', payment: 'usdc-evm', shopper: 'shopper-2', escrow: 'escrow-1' });
      await pauseService('shopper-2', 600);
      try {
        await user.acceptQuote(o.id);
        await user.fund(o.id);
        const now = (await faucet.height()).evm_time;
        await faucet.evmTime(o.quote!.timelock!.t2 - now + 1);
        const before = await usdcOf('user-2');
        const r = await user.refundAfterTimelock(o.id);
        const back = (await usdcOf('user-2')) - before;
        assert(back === BigInt(o.quote!.lock_amount!), `refund ${back} vs lock ${o.quote!.lock_amount}`);
        log(`T2=${o.quote!.timelock!.t2}: user-2 refunded ${back} alone, tx ${r.refundTxid}`);
      } finally {
        await resumeService('shopper-2');
      }
    },
  },
  {
    id: 'i',
    title: '誠実な escrow による USDC の紛争: 配達失敗 → 裁定で返金 → 利用者が連署。裁定の前に誰かが Safe に 1 単位送り付けても執行できる',
    async run({ users, log }) {
      const user = users['user-1'];
      const o = await placeFunded(user, { shopUrl: SAFE_SHOP, region: SHINJUKU, sku: 'FAIL-100', payment: 'usdc-evm', shopper: 'shopper-1', escrow: 'escrow-1' }, 'delivery_failed');
      await user.openDispute(o.id, { claim: 'not_delivered', text: '配送に失敗しました' });
      await until('escrow-1 case open', 60_000, async () => (await admin('escrow-1').case(o.id)).state === 'open');
      // 攻撃: Safe のアドレスは誰でも計算できるので、1 単位送り付けて合計を狂わせようとする
      const safe = (o.funded as { safe: `0x${string}` }).safe;
      const griefer = (await startUser('faucet')).s;
      await faucet.evm(griefer.keys.evmAddress, '1');
      await griefer.evm!.transferUsdc(safe, 1n);
      griefer.stop();
      const lock = BigInt(o.quote!.lock_amount!);
      const total = lock + 1n; // 裁定は署名した時点の残高を分ける（§4.8）
      const fee = (total * 200n) / 10_000n;
      await admin('escrow-1').rule(o.id, { user: String(total - fee), shopper: '0', reason: '配送失敗のため返金' });
      await waitOrder(user, o.id, 'ruled');
      const problems = await user.reviewRuling(o.id);
      assert(problems.length === 0, `ruling review: ${problems.join('; ')}`);
      const before = await usdcOf('user-1');
      await user.countersignRuling(o.id);
      const back = (await usdcOf('user-1')) - before;
      assert(back === total - fee, `refund ${back}, want ${total - fee}`);
      await waitOrder(user, o.id, 'settled');
      log(`USDC dispute ${o.id.slice(0, 8)}: refunded ${back} (escrow fee ${fee}) despite 1 unit of dust`);
    },
  },
  {
    id: 'j',
    title: '買えなかった注文: 店が在庫切れ → shopper が協力的な払い戻しを申し出る → 利用者が確かめて受け入れる（BTC）',
    async run({ users, log }) {
      const user = users['user-2'];
      const o = await placeFunded(user, { shopUrl: SAFE_SHOP, region: SHINJUKU, sku: 'SOLDOUT-100', payment: 'btc-signet', shopper: 'shopper-1', escrow: 'escrow-1' }, 'funded');
      const so = await waitShopper('shopper-1', o.id, ['purchase_failed', 'needs_human', 'cancelled'], 120_000);
      if (so.state === 'needs_human') await admin('shopper-1').resolve(o.id, { action: 'refund' });
      const offered = await until('refund offer reaches the user', 90_000, async () => {
        const x = await user.getOrder(o.id);
        return x?.refundOffer ? x : undefined;
      });
      const problems = await user.reviewRefundOffer(o.id);
      assert(problems.length === 0, `refund offer: ${problems.join('; ')}`);
      const done = await user.acceptRefundOffer(o.id);
      const txid = done.refundTxid ?? done.settledTxid;
      assert(txid, 'refund txid');
      const got = await paidTo(txid, o.request.user_btc_address!);
      const want = BigInt(o.quote!.lock_amount!) - BigInt(o.quote!.payout_fee_reserve ?? '0');
      assert(got >= want - 1000n, `refunded ${got}, want about ${want}`);
      log(`sold out ${offered.id.slice(0, 8)}: shopper ${so.state}, cooperative refund ${got} sats`);
    },
  },
  {
    id: 'k',
    title: 'タイムロック T2（BTC）: shopper が消えたら、T2 を過ぎた後に利用者が単独で取り戻す',
    async run({ users, log }) {
      const user = users['user-2'];
      const q = await requestQuote(user, { shopUrl: SAFE_SHOP, region: SHINJUKU, sku: 'A-100', payment: 'btc-signet', shopper: 'shopper-2', escrow: 'escrow-1' });
      await pauseService('shopper-2', 600);
      try {
        await acceptAndFund(user, q, 'funded');
        const t2 = q.quote!.timelock!.t2;
        let failedEarly = false;
        try {
          await user.refundAfterTimelock(q.id);
        } catch {
          failedEarly = true;
        }
        assert(failedEarly, 'refund before T2 must fail');
        const { btc } = await faucet.height();
        await faucet.mine(t2 - btc + 1);
        const r = await user.refundAfterTimelock(q.id);
        const got = await paidTo(r.refundTxid!, q.request.user_btc_address!);
        log(`T2=${t2}: user-2 refunded ${got} sats alone, tx ${r.refundTxid}`);
      } finally {
        await resumeService('shopper-2');
      }
    },
  },
];

// shopper を「消す」: 管理 API で、ノードのメッセージの送受信と定期処理を止める（libp2p は動いたまま）
async function pauseService(service: string, seconds: number) {
  await admin(service).pause(seconds);
}
async function resumeService(service: string) {
  await admin(service).resume();
}
