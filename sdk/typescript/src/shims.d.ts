// Minimal typings for the two Node modules we touch, so the SDK builds without @types/node.
declare module "node:crypto" {
  export function createHmac(algorithm: string, key: string | Uint8Array): {
    update(data: string | Uint8Array): { digest(encoding: "hex"): string; update(data: string | Uint8Array): any };
  };
  export function timingSafeEqual(a: Uint8Array, b: Uint8Array): boolean;
}
