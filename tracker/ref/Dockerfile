FROM python:3.14-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt && rm -rf /root/.cache
COPY app.py .
ENV PORT=8080 PYTHONUNBUFFERED=1
EXPOSE 8080
STOPSIGNAL SIGTERM
CMD ["python", "-u", "/app/app.py"]
