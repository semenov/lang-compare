import os

PORT = int(os.environ.get("PORT", "8080"))
DATABASE_URL = os.environ.get("DATABASE_URL", "postgres://app:app@localhost:15432/app")
CATALOG_URL = os.environ.get("CATALOG_URL", "http://localhost:9000")
DB_POOL_SIZE = int(os.environ.get("DB_POOL_SIZE", "20"))
WORKERS = int(os.environ.get("WORKERS", "1"))
