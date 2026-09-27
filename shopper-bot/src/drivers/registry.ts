import type { Driver } from "./driver.js";
import { hostOf } from "./driver.js";

/** Maps shop hosts to drivers. Later registrations replace earlier ones. */
export class DriverRegistry {
  private readonly byHost = new Map<string, Driver>();

  register(driver: Driver): this {
    for (const host of driver.hosts) this.byHost.set(host.toLowerCase(), driver);
    return this;
  }

  forUrl(shopUrl: string): Driver | undefined {
    return this.byHost.get(hostOf(shopUrl));
  }

  hosts(): string[] {
    return [...this.byHost.keys()].sort();
  }
}
