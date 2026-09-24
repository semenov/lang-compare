import asyncio
from contextlib import asynccontextmanager
from datetime import datetime

import asyncpg
from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, Response
from pydantic import BaseModel, ConfigDict

from . import config
from .catalog import Catalog, UpstreamError
from .db import Duplicate, Store


class AppError(Exception):
    def __init__(self, status: int, message: str):
        self.status, self.message = status, message


class CreateUser(BaseModel):
    model_config = ConfigDict(strict=True)
    email: str
    name: str


class CreateOrderItem(BaseModel):
    model_config = ConfigDict(strict=True)
    sku: str
    qty: int


class CreateOrder(BaseModel):
    model_config = ConfigDict(strict=True)
    user_id: int
    items: list[CreateOrderItem]


# Response models: FastAPI serializes these straight to JSON bytes via pydantic-core (its fast path).
class User(BaseModel):
    id: int
    email: str
    name: str
    created_at: datetime


class OrderItem(BaseModel):
    sku: str
    name: str
    qty: int
    unit_price_cents: int


class OrderSummary(BaseModel):
    id: int
    user_id: int
    status: str
    total_cents: int
    currency: str
    created_at: datetime


class Order(OrderSummary):
    items: list[OrderItem]


class OrderList(BaseModel):
    orders: list[OrderSummary]
    limit: int
    offset: int


class Health(BaseModel):
    status: str


@asynccontextmanager
async def lifespan(app: FastAPI):
    pool = await asyncpg.create_pool(config.DATABASE_URL, min_size=1, max_size=config.DB_POOL_SIZE)
    app.state.store = Store(pool)
    app.state.catalog = Catalog(config.CATALOG_URL)
    await app.state.catalog.start()
    yield
    await app.state.catalog.close()
    await pool.close()


app = FastAPI(lifespan=lifespan, openapi_url=None)


@app.exception_handler(AppError)
async def app_error(_: Request, e: AppError):
    return JSONResponse({"error": e.message}, status_code=e.status)


@app.exception_handler(RequestValidationError)
async def validation_error(_: Request, e: RequestValidationError):
    err = e.errors()[0]
    return JSONResponse({"error": f"{'.'.join(map(str, err['loc']))}: {err['msg']}"}, status_code=400)


@app.exception_handler(UpstreamError)
async def upstream_error(_: Request, e: UpstreamError):
    return JSONResponse({"error": "upstream error"}, status_code=502)


def parse_id(raw: str) -> int:
    if not raw.isdigit() or len(raw) > 18 or int(raw) <= 0:
        raise AppError(400, "invalid id")
    return int(raw)


def query_int(request: Request, key: str, default: int, lo: int, hi: int) -> int:
    raw = request.query_params.get(key)
    if raw is None:
        return default
    try:
        v = int(raw)
    except ValueError:
        raise AppError(400, f"invalid {key}")
    if not lo <= v <= hi:
        raise AppError(400, f"invalid {key}")
    return v


@app.get("/health")
async def health() -> Health:
    return {"status": "ok"}


@app.post("/users", status_code=201)
async def create_user(body: CreateUser, request: Request) -> User:
    if not 3 <= len(body.email) <= 254 or "@" not in body.email:
        raise AppError(400, "invalid email")
    if not 1 <= len(body.name) <= 100:
        raise AppError(400, "name must be 1..100 chars")
    try:
        return await request.app.state.store.create_user(body.email, body.name)
    except Duplicate:
        raise AppError(409, "email already exists")


@app.get("/users/{user_id}")
async def get_user(user_id: str, request: Request) -> User:
    user = await request.app.state.store.get_user(parse_id(user_id))
    if user is None:
        raise AppError(404, "user not found")
    return user


@app.get("/users/{user_id}/orders")
async def list_orders(user_id: str, request: Request) -> OrderList:
    uid = parse_id(user_id)
    limit = query_int(request, "limit", 20, 1, 100)
    offset = query_int(request, "offset", 0, 0, 2**31 - 1)
    store: Store = request.app.state.store
    exists, orders = await asyncio.gather(store.user_exists(uid), store.list_orders(uid, limit, offset))
    if not exists:
        raise AppError(404, "user not found")
    return {"orders": orders, "limit": limit, "offset": offset}


@app.get("/products/{sku}")
async def proxy_product(sku: str, request: Request):
    try:
        status, body = await request.app.state.catalog.get_raw(sku)
    except Exception:
        raise AppError(502, "upstream unavailable")
    return Response(content=body, status_code=status, media_type="application/json")


@app.post("/orders", status_code=201)
async def create_order(body: CreateOrder, request: Request) -> Order:
    if body.user_id <= 0 or not 1 <= len(body.items) <= 20:
        raise AppError(400, "user_id must be positive and items must have 1..20 entries")
    if any(not i.sku or not 1 <= i.qty <= 1000 for i in body.items):
        raise AppError(400, "each item needs a sku and qty in 1..1000")

    store: Store = request.app.state.store
    catalog: Catalog = request.app.state.catalog
    # user lookup and all catalog lookups run concurrently
    exists, *products = await asyncio.gather(
        store.user_exists(body.user_id), *(catalog.product(i.sku) for i in body.items), return_exceptions=True
    )
    if isinstance(exists, BaseException):
        raise exists
    if not exists:
        raise AppError(404, "user not found")

    total, items = 0, []
    for item, p in zip(body.items, products):
        if isinstance(p, BaseException):
            raise p
        if p is None:
            raise AppError(422, f"unknown sku: {item.sku}")
        if item.qty > p["stock"]:
            raise AppError(422, f"insufficient stock: {item.sku}")
        total += p["price_cents"] * item.qty
        items.append({"sku": item.sku, "name": p["name"], "qty": item.qty, "unit_price_cents": p["price_cents"]})

    return await store.create_order(body.user_id, products[0]["currency"], total, items)


@app.get("/orders/{order_id}")
async def get_order(order_id: str, request: Request) -> Order:
    order = await request.app.state.store.get_order(parse_id(order_id))
    if order is None:
        raise AppError(404, "order not found")
    return order
