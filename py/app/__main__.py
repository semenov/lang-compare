import uvicorn

from . import config

if __name__ == "__main__":
    uvicorn.run(
        "app.main:app",
        host="0.0.0.0",
        port=config.PORT,
        workers=config.WORKERS,
        loop="uvloop",
        http="httptools",
        access_log=False,
        log_level="warning",
    )
