# Bifrost control-plane image.
#
# Two stages: a throwaway Go builder, and a UBI9-micro runtime carrying only
# the static binary and a CA bundle. UBI9-micro is the posture docs/SPEC.md
# records (inherited from predecessor ADR-0008); it is a real, pullable Red Hat base
# rather than an aspiration, so this follows it rather than substituting
# scratch/distroless.
#
# Both bases are pinned by digest, not tag: a tag is a moving target, and
# "the image we tested" and "the image that ships" have to be the same bytes.
# Refresh a digest deliberately, in its own commit.
#
# Build:
#   docker build -t bifrost:dev .
#   docker build --build-arg VERSION=1.2.3 -t bifrost:1.2.3 .
#
# Run (see the bind note near USER below — the default bind is loopback, which
# inside a container means unreachable from outside it):
#   docker run --rm bifrost:dev --help
#   docker run --rm -p 8484:8484 bifrost:dev serve --local-auth \
#     --store sqlite --db /data/bifrost.db --bind 0.0.0.0:8484

# --- build -------------------------------------------------------------------
FROM golang@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468 AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# graph. go.sum is copied with go.mod because `go mod download` verifies it.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION is stamped into the binary the same way a release would
# (cmd/bifrost/main.go's buildVersion is documented as -ldflags-overridable).
# The default matches the in-source value so an unstamped build is not a lie.
ARG VERSION=0.0.1

# CGO_ENABLED=0 is the shipped-binary rule from docs/SPEC.md, and it is
# satisfiable here because the SQLite driver is pure Go (modernc.org/sqlite);
# cgo is only ever needed by `go test -race`, which ships nothing.
#
# -trimpath keeps build-host paths out of the binary; -s -w drop the symbol
# and DWARF tables, which is size, not security.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$(go env GOARCH) \
    go build -trimpath -ldflags "-s -w -X main.buildVersion=${VERSION}" \
    -o /out/bifrost ./cmd/bifrost

# A passwd entry for the runtime UID. Without one, os/user lookups and anything
# reading /etc/passwd see an unknown uid; ubi-micro ships no useradd to do this
# in the final stage, so it is prepared here and copied in.
RUN printf 'bifrost:x:1001:0:bifrost:/:/sbin/nologin\n' >> /etc/passwd \
 && cp /etc/passwd /out/passwd

# --- runtime -----------------------------------------------------------------
FROM registry.access.redhat.com/ubi9/ubi-micro@sha256:f332c99eb8f798a8486821c91937f10ad64ee83d7e739303be2df051040918f6

# ubi-micro ships NO CA bundle — /etc/pki/tls/certs and /etc/ssl/certs do not
# exist in it. That is not cosmetic: OIDC discovery and the JWKS fetch are
# HTTPS, so without this the control plane cannot validate a single token
# against a real IdP, and fails at runtime rather than at build time.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt
COPY --from=build /out/passwd /etc/passwd
COPY --from=build /out/bifrost /usr/local/bin/bifrost

# Non-root, and group 0 rather than a private group: an OpenShift-style runtime
# assigns an arbitrary UID but keeps gid 0, so group-readable files stay
# readable when the declared UID is overridden.
USER 1001:0

# The default bind is 127.0.0.1:8484, which inside a container is reachable
# only from inside the container. Serving to anything else means passing
# --bind 0.0.0.0:8484 — and Bifrost's fail-closed guard then requires real
# authentication (--local-auth or --auth-config) before it will accept a
# non-loopback bind. That refusal is the intended behaviour, not a
# misconfiguration to work around.
EXPOSE 8484

ENTRYPOINT ["/usr/local/bin/bifrost"]
CMD ["--help"]
