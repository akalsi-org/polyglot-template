FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
ENV CI=1

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    wget \
    git \
    zstd \
    unzip \
    tar \
    gzip \
    xz-utils \
    procps \
    python3 \
    build-essential \
    sudo \
    && rm -rf /var/lib/apt/lists/*

# Grant passwordless sudo for unprivileged userns configuration if needed
RUN echo "ubuntu ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

USER ubuntu
WORKDIR /workspace
