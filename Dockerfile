FROM golang:1.23-alpine AS build

WORKDIR /src
# No third-party modules, so there is no dependency layer to cache and no
# module download step that can fail.
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/opamela ./cmd/opamela

FROM alpine:3.20

# git is a runtime dependency: the mirror keeps a checkout of opam-repository.
# ca-certificates is needed to fetch archives over HTTPS.
RUN apk add --no-cache git ca-certificates \
    && adduser -D -H -u 65532 opamela \
    && mkdir -p /var/lib/opamela \
    && chown 65532:65532 /var/lib/opamela

COPY --from=build /out/opamela /usr/local/bin/opamela

USER 65532:65532
VOLUME /var/lib/opamela
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/opamela"]
