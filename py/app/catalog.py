from urllib.parse import quote

import aiohttp
import json


class UpstreamError(Exception):
    pass


class Catalog:
    def __init__(self, base_url: str):
        self.base_url = base_url
        self.session: aiohttp.ClientSession | None = None

    async def start(self) -> None:
        connector = aiohttp.TCPConnector(limit=512, limit_per_host=512)
        self.session = aiohttp.ClientSession(connector=connector, timeout=aiohttp.ClientTimeout(total=10))

    async def close(self) -> None:
        if self.session:
            await self.session.close()

    async def get_raw(self, sku: str) -> tuple[int, bytes]:
        """Raw pass-through fetch for the proxy endpoint."""
        async with self.session.get(f"{self.base_url}/products/{quote(sku, safe='')}") as resp:
            return resp.status, await resp.read()

    async def product(self, sku: str) -> dict | None:
        """Returns the product, or None if the catalog says 404."""
        try:
            status, body = await self.get_raw(sku)
        except aiohttp.ClientError as e:
            raise UpstreamError(str(e)) from e
        if status == 404:
            return None
        if status != 200:
            raise UpstreamError(f"catalog returned {status}")
        return json.loads(body)
