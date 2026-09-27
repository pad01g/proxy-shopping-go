import type { PurchaseRequest } from "../src/types.js";

export function purchaseRequest(over: Partial<PurchaseRequest> = {}): PurchaseRequest {
  return {
    request_id: "req-1",
    order_id: "0123456789abcdef0123456789abcdef",
    shop_url: "https://safe-shop.test/",
    items: [{ sku: "A-100", qty: 1 }],
    shipping: { name: "山田太郎", postal_code: "160-0022", address: "東京都新宿区新宿3-1-1", phone: "03-0000-0000" },
    payment_ref: "card:default",
    max_amount: { amount: "5000", currency: "JPY" },
    ...over,
  };
}
