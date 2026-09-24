import asyncpg

ORDER_COLS = "id, user_id, status, total_cents, currency, created_at"


class Duplicate(Exception):
    pass


class Store:
    def __init__(self, pool: asyncpg.Pool):
        self.pool = pool

    async def create_user(self, email: str, name: str) -> dict:
        try:
            row = await self.pool.fetchrow(
                "INSERT INTO users (email, name) VALUES ($1, $2) RETURNING id, email, name, created_at", email, name
            )
        except asyncpg.UniqueViolationError as e:
            raise Duplicate() from e
        return dict(row)

    async def get_user(self, user_id: int) -> dict | None:
        row = await self.pool.fetchrow("SELECT id, email, name, created_at FROM users WHERE id = $1", user_id)
        return dict(row) if row else None

    async def user_exists(self, user_id: int) -> bool:
        return await self.pool.fetchval("SELECT 1 FROM users WHERE id = $1", user_id) is not None

    async def create_order(self, user_id: int, currency: str, total: int, items: list[dict]) -> dict:
        async with self.pool.acquire() as conn, conn.transaction():
            order = dict(
                await conn.fetchrow(
                    f"INSERT INTO orders (user_id, total_cents, currency) VALUES ($1, $2, $3) RETURNING {ORDER_COLS}",
                    user_id, total, currency,
                )
            )
            await conn.execute(
                """INSERT INTO order_items (order_id, sku, name, qty, unit_price_cents)
                   SELECT $1, * FROM unnest($2::text[], $3::text[], $4::int[], $5::bigint[])""",
                order["id"],
                [i["sku"] for i in items],
                [i["name"] for i in items],
                [i["qty"] for i in items],
                [i["unit_price_cents"] for i in items],
            )
        order["items"] = items
        return order

    async def get_order(self, order_id: int) -> dict | None:
        row = await self.pool.fetchrow(f"SELECT {ORDER_COLS} FROM orders WHERE id = $1", order_id)
        if row is None:
            return None
        items = await self.pool.fetch(
            "SELECT sku, name, qty, unit_price_cents FROM order_items WHERE order_id = $1 ORDER BY id", order_id
        )
        return {**dict(row), "items": [dict(i) for i in items]}

    async def list_orders(self, user_id: int, limit: int, offset: int) -> list[dict]:
        rows = await self.pool.fetch(
            f"SELECT {ORDER_COLS} FROM orders WHERE user_id = $1 ORDER BY id DESC LIMIT $2 OFFSET $3",
            user_id, limit, offset,
        )
        return [dict(r) for r in rows]
