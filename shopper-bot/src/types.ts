// Wire types of the shopper-bot API (spec §9). They mirror schema/*.json;
// every request and response is validated against those schemas.

export interface Money {
  amount: string; // decimal string, never a JSON number
  currency: string;
}

export interface Address {
  name: string;
  postal_code: string;
  address: string;
  phone: string;
}

export type EvidenceKind = "screenshot" | "receipt" | "html" | "json";

export interface Evidence {
  kind: EvidenceKind;
  sha256: string;
  mime: string;
  data_b64?: string;
}

export interface PurchaseItem {
  sku: string;
  qty: number;
}

/** "card:<name>" names a card in the bot's own card file; "cash" means paying at a counter. */
export type PaymentRef = `card:${string}` | "cash";

export interface PurchaseRequest {
  request_id: string;
  order_id: string;
  shop_url: string;
  items: PurchaseItem[];
  shipping: Address;
  payment_ref: PaymentRef;
  max_amount: Money;
}

export type PurchaseStatus = "ok" | "failed" | "needs_human";

export interface PurchaseResult {
  request_id: string;
  status: PurchaseStatus;
  shop_order_id?: string;
  total?: Money;
  evidence: Evidence[];
  error?: string;
}

export interface TrackingQuery {
  shop_url: string;
  shop_order_id: string;
}

export type ShipmentStatus = "processing" | "shipped" | "delivered" | "failed";

export interface TrackingStatus {
  status: ShipmentStatus;
  carrier?: string;
  tracking_no?: string;
  updated_at: number; // UNIX seconds
  evidence: Evidence[];
}
