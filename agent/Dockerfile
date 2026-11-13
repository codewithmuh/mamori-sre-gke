# The incident agent image. Build context: agent/ only, so nothing else from the repository can
# end up inside (in particular chaos/faults/, the evaluation's answer key).
#   docker build -t mamori-agent:dev agent
#
# Runtime: distroless Python (Debian 13, Python 3.13), non-root, no shell, no package manager.
# Works with a read-only root filesystem: nothing is written at run time (bytecode is compiled
# here, PYTHONDONTWRITEBYTECODE stops any more).

FROM ghcr.io/astral-sh/uv:0.13.6@sha256:af22a52f5eafef3d78e551599d405c02df61919d18028f0cea8eda93bc9986d0 AS uv

# 3.13 to match the runtime's /usr/bin/python3.13 (same ABI for the compiled wheels).
FROM python:3.13.18-slim-trixie@sha256:ae14d4ce8771606477a81446e7ef429edd6762305fc50c84507c1041fa8285d1 AS build
COPY --from=uv /uv /usr/local/bin/uv
ENV UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    UV_PYTHON_DOWNLOADS=never \
    UV_PROJECT_ENVIRONMENT=/opt/venv
WORKDIR /src
COPY pyproject.toml uv.lock ./
# Runtime dependencies only, exactly as locked (no dev tools, no network resolution).
RUN uv sync --frozen --no-dev --no-install-project
COPY src/ ./src/
RUN python -m compileall -q src

FROM gcr.io/distroless/python3-debian13:nonroot@sha256:6c4cb3bfa4b555429dd19541796a6ba6b30c84675b460bd0bd5072f7d5bdae71
COPY --from=build /opt/venv/lib/python3.13/site-packages /app/site-packages
COPY --from=build /src/src /app/src
ENV PYTHONPATH=/app/src:/app/site-packages \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1
WORKDIR /app
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/bin/python3.13", "-m", "mamori_agent"]
CMD ["serve"]
