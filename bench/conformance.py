#!/usr/bin/env python3
"""Black-box conformance test against SPEC.md. Usage: conformance.py [base_url]"""
import json, sys, uuid, concurrent.futures, urllib.request, urllib.error

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8080"
fails = 0

def req(method, path, body=None, raw=None):
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    r = urllib.request.Request(BASE + path, data=data, method=method,
                               headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(r, timeout=5) as resp:
            return resp.status, resp.headers.get("Content-Type", ""), json.loads(resp.read() or b"null")
    except urllib.error.HTTPError as e:
        b = e.read()
        try: b = json.loads(b)
        except Exception: pass
        return e.code, e.headers.get("Content-Type", ""), b

def check(name, cond, info=""):
    global fails
    if not cond:
        fails += 1
        print(f"FAIL {name} {info}")
    else:
        print(f"ok   {name}")

s, ct, b = req("GET", "/health")
check("health", s == 200 and b == {"status": "ok"} and "application/json" in ct, (s, ct, b))

email = f"t-{uuid.uuid4().hex[:10]}@example.com"
s, _, u = req("POST", "/users", {"email": email, "name": "Tester"})
check("create user", s == 201 and u["email"] == email and u["name"] == "Tester" and isinstance(u["id"], int) and "T" in u["created_at"], (s, u))
s, _, b = req("POST", "/users", {"email": email, "name": "Tester"})
check("dup user 409", s == 409 and b == {"error": "email already exists"}, (s, b))
for bad in [{"email": "noat", "name": "x"}, {"email": "a@b.c", "name": ""}, {"email": "a@b.c"}, {"email": 5, "name": "x"}, {"email": "a@b.c", "name": "x" * 101}]:
    s, _, b = req("POST", "/users", bad)
    check(f"bad user 400 {bad!s:.40}", s == 400 and "error" in b, (s, b))
s, _, b = req("POST", "/users", raw=b"{not json")
check("malformed json 400", s == 400, (s, b))

s, _, b = req("GET", f"/users/{u['id']}")
check("get user", s == 200 and b == u, (s, b, u))
s, _, b = req("GET", "/users/999999999")
check("get user 404", s == 404 and "error" in b, (s, b))
s, _, b = req("GET", "/users/abc")
check("get user 400", s == 400, (s, b))

s, ct, b = req("GET", "/products/SKU-42")
check("proxy 200", s == 200 and b == {"sku": "SKU-42", "name": "Product 42", "price_cents": 1654, "currency": "USD", "stock": 542} and "application/json" in ct, (s, b))
s, _, b = req("GET", "/products/NOPE")
check("proxy 404 passthrough", s == 404 and b == {"error": "not found"}, (s, b))

s, _, o = req("POST", "/orders", {"user_id": u["id"], "items": [{"sku": "SKU-1", "qty": 2}, {"sku": "SKU-42", "qty": 1}]})
exp_total = 2 * 137 + 1654
check("create order", s == 201 and o["user_id"] == u["id"] and o["status"] == "created" and o["total_cents"] == exp_total
      and o["currency"] == "USD" and o["items"] == [{"sku": "SKU-1", "name": "Product 1", "qty": 2, "unit_price_cents": 137},
                                                  {"sku": "SKU-42", "name": "Product 42", "qty": 1, "unit_price_cents": 1654}], (s, o))
s, _, b = req("GET", f"/orders/{o['id']}")
check("get order", s == 200 and b == o, (s, b, o))
s, _, b = req("GET", "/orders/999999999")
check("get order 404", s == 404, (s, b))
s, _, b = req("POST", "/orders", {"user_id": 999999999, "items": [{"sku": "SKU-1", "qty": 1}]})
check("order unknown user 404", s == 404 and b == {"error": "user not found"}, (s, b))
s, _, b = req("POST", "/orders", {"user_id": u["id"], "items": [{"sku": "SKU-1", "qty": 1}, {"sku": "BAD-1", "qty": 1}]})
check("order unknown sku 422", s == 422 and b == {"error": "unknown sku: BAD-1"}, (s, b))
s, _, b = req("POST", "/orders", {"user_id": u["id"], "items": [{"sku": "SKU-1", "qty": 1000}]})
check("order stock 422", s == 422 and b == {"error": "insufficient stock: SKU-1"}, (s, b))
for bad in [{"user_id": u["id"], "items": []}, {"user_id": 0, "items": [{"sku": "SKU-1", "qty": 1}]},
            {"user_id": u["id"], "items": [{"sku": "SKU-1", "qty": 0}]}, {"user_id": u["id"], "items": [{"sku": "", "qty": 1}]},
            {"user_id": u["id"], "items": [{"sku": "SKU-1", "qty": 1}] * 21}, {"user_id": "1", "items": [{"sku": "SKU-1", "qty": 1}]}]:
    s, _, b = req("POST", "/orders", bad)
    check(f"bad order 400 {bad!s:.50}", s == 400, (s, b))

s, _, o2 = req("POST", "/orders", {"user_id": u["id"], "items": [{"sku": "SKU-7", "qty": 3}]})
s, _, b = req("GET", f"/users/{u['id']}/orders")
check("list orders", s == 200 and b["limit"] == 20 and b["offset"] == 0 and [x["id"] for x in b["orders"]] == [o2["id"], o["id"]]
      and "items" not in b["orders"][0] and b["orders"][1]["total_cents"] == exp_total, (s, b))
s, _, b = req("GET", f"/users/{u['id']}/orders?limit=1&offset=1")
check("list orders paging", s == 200 and [x["id"] for x in b["orders"]] == [o["id"]] and b["limit"] == 1 and b["offset"] == 1, (s, b))
for q in ["limit=0", "limit=101", "offset=-1", "limit=abc"]:
    s, _, b = req("GET", f"/users/{u['id']}/orders?{q}")
    check(f"list bad {q}", s == 400, (s, b))
s, _, b = req("GET", "/users/999999999/orders")
check("list unknown user 404", s == 404, (s, b))
s, _, b = req("GET", "/users/5000/orders")
check("list seeded user", s == 200 and len(b["orders"]) == 10, (s, len(b.get("orders", []))))

# concurrency sanity: 200 parallel order creations
def mk(_):
    return req("POST", "/orders", {"user_id": 1 + _ % 100, "items": [{"sku": "SKU-3", "qty": 1}, {"sku": "SKU-4", "qty": 1}]})[0]
with concurrent.futures.ThreadPoolExecutor(32) as ex:
    codes = list(ex.map(mk, range(200)))
check("200 concurrent orders", codes.count(201) == 200, set(codes))

print("\nALL PASSED" if fails == 0 else f"\n{fails} FAILED")
sys.exit(1 if fails else 0)
