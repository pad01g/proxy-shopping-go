// 注文を出して、見積まで進める共通の手順。
import type { Payment, UserClient, UserOrder } from '@proxy-shopping/core';
import { ADDRESS, assert, pk, waitOrder, type LabName } from './lab.js';

export interface OrderSpec {
  shopUrl: string;
  region: string;
  sku: string;
  qty?: number;
  payment: Payment;
  shopper: LabName;
  escrow: LabName;
}

/** 候補から指定の shopper × escrow を選んで注文し、見積（または拒否）を待つ */
export async function requestQuote(user: UserClient, o: OrderSpec): Promise<UserOrder> {
  const offers = await user.discoverOffers({ shopUrl: o.shopUrl, region: o.region, payment: o.payment, refresh: true });
  const offer = offers.find((x) => x.entry.shopper === pk(o.shopper) && x.entry.escrow === pk(o.escrow));
  assert(offer, `offer ${o.shopper} × ${o.escrow} for ${o.region} (have ${offers.length})`);
  const order = await user.createOrder({
    offer,
    shopUrl: o.shopUrl,
    region: o.region,
    items: [{ sku: o.sku, qty: o.qty ?? 1 }],
    payment: o.payment,
    address: ADDRESS,
  });
  return waitOrder(user, order.id, ['quoted', 'rejected']);
}

/** 見積を承諾して入金し、配送の結果（届いた / 失敗）まで待つ */
export async function acceptAndFund(user: UserClient, quoted: UserOrder, until: 'delivered' | 'delivery_failed' | 'funded' = 'delivered'): Promise<UserOrder> {
  assert(quoted.status === 'quoted', `quoted, got ${quoted.status}: ${quoted.quote?.reject_reason ?? ''} ${quoted.quote?.detail ?? ''}`);
  assert(quoted.quoteCheck?.ok, `quote check: ${quoted.quoteCheck?.errors.join('; ')}`);
  await user.acceptQuote(quoted.id);
  await user.fund(quoted.id);
  return waitOrder(user, quoted.id, until, 120_000);
}

export async function placeFunded(user: UserClient, o: OrderSpec, until: 'delivered' | 'delivery_failed' | 'funded' = 'delivered'): Promise<UserOrder> {
  return acceptAndFund(user, await requestQuote(user, o), until);
}
