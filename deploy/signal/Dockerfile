# Minimal signal-cli sidecar image (Phase 4 Task 5, PRD §20/§20.1): no
# official AsamK/signal-cli Docker image exists on any registry (see
# deploy/signal/README.md and the upstream project's own README, which
# only documents community-maintained images of unclear provenance) —
# this project pins the exact upstream GitHub release tarball by its
# published version and a recorded sha256, the same "vendor a pinned,
# checksummed artifact rather than trust a moving image tag" posture
# `make vendor-js`/`make vendor-fonts` already use for htmx/Alpine/
# fonts (see README.md's own comment on that).
#
# The "Linux-native" release asset is a GraalVM native-image build —
# self-contained (no JRE needed), dynamically linked against glibc
# only, which is why the runtime stage below is a plain glibc base
# rather than a JRE image.
FROM debian:bookworm-slim AS fetch
ARG SIGNAL_CLI_VERSION=0.14.7
ARG SIGNAL_CLI_SHA256=0fe065294adcf35df4c249b635d0ce57de7765d4fec660bffaa2e7f0549d4e5f
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
WORKDIR /tmp
RUN curl -fsSL -o signal-cli.tar.gz \
      "https://github.com/AsamK/signal-cli/releases/download/v${SIGNAL_CLI_VERSION}/signal-cli-${SIGNAL_CLI_VERSION}-Linux-native.tar.gz" \
    && echo "${SIGNAL_CLI_SHA256}  signal-cli.tar.gz" | sha256sum -c - \
    && tar xzf signal-cli.tar.gz \
    && chmod +x signal-cli

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=fetch /tmp/signal-cli /usr/local/bin/signal-cli
ENTRYPOINT ["signal-cli"]
