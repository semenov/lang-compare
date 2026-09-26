import { Pool } from "undici";
import { config } from "./config.js";

export interface Product {
  sku: string;
  name: string;
  price_cents: number;
  currency: string;
  stock: number;
}

export class UpstreamError extends Error {}

interface UpstreamResponse {
  status: number;
  body: Buffer;
}

type Get = (path: string) => Promise<UpstreamResponse>;

// undici's Pool is the fastest client on Node but doesn't work on Bun; Bun's native fetch is the idiomatic choice there.
const get: Get =
  "Bun" in globalThis
    ? async (path) => {
        const res = await fetch(config.catalogUrl + path);
        return { status: res.status, body: Buffer.from(await res.arrayBuffer()) };
      }
    : (() => {
        const pool = new Pool(config.catalogUrl, { connections: 128, pipelining: 1 });
        return async (path: string) => {
          const res = await pool.request({ method: "GET", path });
          return { status: res.statusCode, body: Buffer.from(await res.body.arrayBuffer()) };
        };
      })();

/** Raw pass-through fetch, used by the proxy endpoint. */
export function fetchRaw(sku: string): Promise<UpstreamResponse> {
  return get(`/products/${encodeURIComponent(sku)}`);
}

/** Returns the product, or null if the catalog says 404. */
export async function getProduct(sku: string): Promise<Product | null> {
  let res;
  try {
    res = await fetchRaw(sku);
  } catch (e) {
    throw new UpstreamError(String(e));
  }
  if (res.status === 404) return null;
  if (res.status !== 200) throw new UpstreamError(`catalog returned ${res.status}`);
  return JSON.parse(res.body.toString()) as Product;
}
