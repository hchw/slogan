# The Laya sidecar is pinned at build time. Rebuild with a reviewed package
# version and record the image digest in the deployment release manifest.
FROM python:3.11-slim
ARG LAYA_VERSION=0.3.20
ENV PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && python -m pip install --upgrade pip \
    && python -m pip install "laya==${LAYA_VERSION}"
COPY entrypoint.sh /entrypoint.sh
RUN chmod 0555 /entrypoint.sh
EXPOSE 8000
ENTRYPOINT ["/entrypoint.sh"]
CMD ["laya-serve", "--host", "0.0.0.0", "--port", "8000"]
