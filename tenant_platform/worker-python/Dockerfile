# Build context must be the project root so legacy files are accessible:
#   podman build -f tenant_platform/worker-python/Dockerfile -t ga-worker .
FROM python:3.11-slim

WORKDIR /ga

# Install core dependencies first for layer caching.
COPY pyproject.toml .
RUN pip install --no-cache-dir .

# Copy legacy runtime into a read-only image layer.
COPY agentmain.py ga.py llmcore.py agent_loop.py simphtml.py /ga/legacy/
COPY plugins /ga/legacy/plugins
COPY assets /ga/legacy/assets

# Copy worker-python sources and generated protobuf bindings.
COPY tenant_platform/worker-python/src /ga/worker-python/src

ENV GA_LEGACY_ROOT=/ga/legacy
ENV PYTHONPATH=/ga/worker-python/src:/ga/legacy

# Mount points for bind-mounted config, runtime data, and gRPC socket.
RUN mkdir -p /ga/config /ga/runtime /ga/rpc

ENTRYPOINT ["python", "-m", "ga_worker.entrypoint", "--listen", "unix:/ga/rpc/worker.sock"]
