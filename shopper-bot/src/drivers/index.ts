import { CardShopDriver } from "./cardShop.js";
import { CashStoreDriver } from "./cashStore.js";
import { DriverRegistry } from "./registry.js";

/** The drivers the lab bot runs with. risky-shop.test is deliberately absent. */
export function defaultRegistry(): DriverRegistry {
  return new DriverRegistry()
    .register(new CardShopDriver({ hosts: ["safe-shop.test", "us-shop.test"], gatewayHost: "cardgw.test" }))
    .register(new CashStoreDriver(["cash-store.test"]));
}
