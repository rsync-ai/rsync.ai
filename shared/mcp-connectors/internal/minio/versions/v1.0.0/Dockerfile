FROM python:3.13-slim

LABEL org.opencontainers.image.title="MinIO MCP (Internal)"
LABEL org.opencontainers.image.description="Internal MinIO staging connector (S3-compatible claim-check storage)"
LABEL org.opencontainers.image.version="1.0.0"
LABEL ai.rsync.connector.type="minio"
LABEL ai.rsync.connector.category="cloud_storage"
LABEL mcp.managed="true"

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY . /app/

# The licence attributions for what this image ships. A repo-root file this build's
# context cannot reach, so it comes through the `legal` named context (the repo root).
COPY --from=legal THIRD_PARTY_NOTICES.md /licenses/THIRD_PARTY_NOTICES.md

ENV MCP_HTTP_MODE=true
ENV DOCKER_CONTAINER=true
ENV MCP_PORT=8000
ENV PORT=8000

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
  CMD curl -f http://localhost:8000/health || exit 1

CMD ["python", "connector.py"]


