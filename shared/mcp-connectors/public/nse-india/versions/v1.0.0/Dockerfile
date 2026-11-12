FROM python:3.13-slim

LABEL org.opencontainers.image.title="NSE India MCP"
LABEL org.opencontainers.image.description="Source for the National Stock Exchange of India's public end-of-day data (prices, delivery, indices, participant OI, corporate actions, deals, FII/DII)"
LABEL org.opencontainers.image.version="1.0.0"
LABEL ai.rsync.connector.type="nse-india"
LABEL ai.rsync.connector.category="api_saas"
LABEL mcp.managed="true"

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY . /app/

ENV MCP_HTTP_MODE=true
ENV DOCKER_CONTAINER=true
ENV MCP_PORT=8000
ENV PORT=8000

# Run as an unprivileged user (defense-in-depth: connectors process untrusted
# upstream data, so least privilege limits blast radius). uid 1000 matches the
# connector-fs-init convention; port 8000 is unprivileged so no extra caps needed.
RUN useradd -m -u 1000 appuser && chown -R appuser:appuser /app
USER appuser

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
  CMD curl -f http://localhost:8000/health || exit 1

CMD ["python", "connector.py"]
