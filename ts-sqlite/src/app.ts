import Fastify, { FastifyError, FastifyReply } from "fastify";
import * as catalog from "./catalog.js";
import * as repo from "./db.js";

class HttpError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

const idParam = {
  type: "object",
  required: ["id"],
  properties: { id: { type: "string", pattern: "^[1-9][0-9]{0,17}$" } },
} as const;

const createUserBody = {
  type: "object",
  required: ["email", "name"],
  properties: {
    email: { type: "string", minLength: 3, maxLength: 254, pattern: "@" },
    name: { type: "string", minLength: 1, maxLength: 100 },
  },
} as const;

const createOrderBody = {
  type: "object",
  required: ["user_id", "items"],
  properties: {
    user_id: { type: "integer", minimum: 1 },
    items: {
      type: "array",
      minItems: 1,
      maxItems: 20,
      items: {
        type: "object",
        required: ["sku", "qty"],
        properties: {
          sku: { type: "string", minLength: 1 },
          qty: { type: "integer", minimum: 1, maximum: 1000 },
        },
      },
    },
  },
} as const;

const listQuery = {
  type: "object",
  properties: {
    limit: { type: "string", pattern: "^[0-9]{1,3}$" },
    offset: { type: "string", pattern: "^[0-9]{1,9}$" },
  },
} as const;

const userSchema = {
  type: "object",
  properties: {
    id: { type: "integer" },
    email: { type: "string" },
    name: { type: "string" },
    created_at: { type: "string" },
  },
} as const;

const orderProps = {
  id: { type: "integer" },
  user_id: { type: "integer" },
  status: { type: "string" },
  total_cents: { type: "integer" },
  currency: { type: "string" },
  created_at: { type: "string" },
} as const;

const orderSchema = {
  type: "object",
  properties: {
    ...orderProps,
    items: {
      type: "array",
      items: {
        type: "object",
        properties: {
          sku: { type: "string" },
          name: { type: "string" },
          qty: { type: "integer" },
          unit_price_cents: { type: "integer" },
        },
      },
    },
  },
} as const;

const orderListSchema = {
  type: "object",
  properties: {
    orders: { type: "array", items: { type: "object", properties: orderProps } },
    limit: { type: "integer" },
    offset: { type: "integer" },
  },
} as const;

export function buildApp() {
  const app = Fastify({
    logger: { level: "error" },
    ajv: { customOptions: { coerceTypes: false, allErrors: false } },
  });

  app.setErrorHandler((err: FastifyError, _req, reply: FastifyReply) => {
    if (err instanceof HttpError) return reply.code(err.status).send({ error: err.message });
    if (err.validation || err.statusCode === 400 || err.code === "FST_ERR_CTP_INVALID_JSON_BODY") {
      return reply.code(400).send({ error: err.message });
    }
    if (err instanceof catalog.UpstreamError) return reply.code(502).send({ error: "upstream error" });
    app.log.error(err);
    return reply.code(500).send({ error: "internal error" });
  });
  app.setNotFoundHandler((_req, reply) => reply.code(404).send({ error: "not found" }));

  app.get("/health", async () => ({ status: "ok" }));

  app.post<{ Body: { email: string; name: string } }>(
    "/users",
    { schema: { body: createUserBody, response: { 201: userSchema } } },
    async (req, reply) => {
      const user = await repo.insertUser(req.body.email, req.body.name);
      if (!user) throw new HttpError(409, "email already exists");
      return reply.code(201).send(user);
    },
  );

  app.get<{ Params: { id: string } }>(
    "/users/:id",
    { schema: { params: idParam, response: { 200: userSchema } } },
    async (req) => {
      const user = await repo.getUser(Number(req.params.id));
      if (!user) throw new HttpError(404, "user not found");
      return user;
    },
  );

  app.get<{ Params: { id: string }; Querystring: { limit?: string; offset?: string } }>(
    "/users/:id/orders",
    { schema: { params: idParam, querystring: listQuery, response: { 200: orderListSchema } } },
    async (req) => {
      const limit = req.query.limit === undefined ? 20 : Number(req.query.limit);
      const offset = req.query.offset === undefined ? 0 : Number(req.query.offset);
      if (limit < 1 || limit > 100) throw new HttpError(400, "limit must be 1..100");
      const userId = Number(req.params.id);
      const [exists, orders] = await Promise.all([repo.userExists(userId), repo.listOrders(userId, limit, offset)]);
      if (!exists) throw new HttpError(404, "user not found");
      return { orders, limit, offset };
    },
  );

  app.get<{ Params: { sku: string } }>("/products/:sku", async (req, reply) => {
    let res;
    try {
      res = await catalog.fetchRaw(req.params.sku);
    } catch {
      throw new HttpError(502, "upstream unavailable");
    }
    return reply.code(res.status).type("application/json").send(res.body);
  });

  app.post<{ Body: { user_id: number; items: { sku: string; qty: number }[] } }>(
    "/orders",
    { schema: { body: createOrderBody, response: { 201: orderSchema } } },
    async (req, reply) => {
      const { user_id, items } = req.body;
      const [exists, products] = await Promise.all([
        repo.userExists(user_id),
        Promise.all(items.map((i) => catalog.getProduct(i.sku))),
      ]);
      if (!exists) throw new HttpError(404, "user not found");

      let total = 0;
      const orderItems: repo.OrderItem[] = items.map((item, idx) => {
        const p = products[idx];
        if (!p) throw new HttpError(422, `unknown sku: ${item.sku}`);
        if (item.qty > p.stock) throw new HttpError(422, `insufficient stock: ${item.sku}`);
        total += p.price_cents * item.qty;
        return { sku: item.sku, name: p.name, qty: item.qty, unit_price_cents: p.price_cents };
      });
      const currency = products[0]!.currency;

      const order = await repo.createOrder(user_id, currency, total, orderItems);
      return reply.code(201).send(order);
    },
  );

  app.get<{ Params: { id: string } }>(
    "/orders/:id",
    { schema: { params: idParam, response: { 200: orderSchema } } },
    async (req) => {
      const order = await repo.getOrder(Number(req.params.id));
      if (!order) throw new HttpError(404, "order not found");
      return order;
    },
  );

  return app;
}
