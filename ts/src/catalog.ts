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

const pool = new Pool(config.catalogUrl, { connections: 128, pipelining: 1 });

/** Raw pass-through fetch, used by the proxy endpoint. */
export async function fetchRaw(sku: string): Promise<{ status: number; body: Buffer }> {
  const res = await pool.request({ method: "GET", path: `/products/${encodeURIComponent(sku)}` });
  return { status: res.statusCode, body: Buffer.from(await res.body.arrayBuffer()) };
}

/** Returns the product, or null if the catalog says 404. */
export async function getProduct(sku: string): Promise<Product | null> {
  let res;
  try {
    res = await pool.request({ method: "GET", path: `/products/${encodeURIComponent(sku)}` });
  } catch (e) {
    throw new UpstreamError(String(e));
  }
  if (res.statusCode === 404) {
    await res.body.dump();
    return null;
  }
  if (res.statusCode !== 200) {
    await res.body.dump();
    throw new UpstreamError(`catalog returned ${res.statusCode}`);
  }
  return (await res.body.json()) as Product;
}
