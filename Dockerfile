# The release image, built by goreleaser (dockers_v2) from the release
# binaries; it compiles nothing. FROM scratch: no shell, no cp. TLS roots
# are compiled into the binary (x509roots/fallback).
#
#   docker run --rm -v email-data:/data ghcr.io/james-tronai/email-cli:<tag> version
#   initContainer: email self-install --dest /opt/data/.local/bin
FROM scratch
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/email /email
COPY LICENSE /LICENSE
# Config, accounts and state default to /data/.config/email and
# /data/.local/state/email; mount a volume there (or set EMAIL_CONFIG_DIR
# and EMAIL_STATE_DIR).
ENV HOME=/data
USER 65532:65532
ENTRYPOINT ["/email"]
CMD ["--help"]
