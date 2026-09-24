FROM python:3.14-slim AS build
WORKDIR /app
RUN python -m venv /venv
COPY requirements.txt .
RUN /venv/bin/pip install --no-cache-dir -r requirements.txt

FROM python:3.14-slim
WORKDIR /app
COPY --from=build /venv /venv
COPY app ./app
USER nobody
ENV PYTHONUNBUFFERED=1
CMD ["/venv/bin/python", "-m", "app"]
