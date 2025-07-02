# Build a go app into a minimal docker image with timezone support and SSL cert chains.
FROM golang:latest@sha256:20a022e5112a144aa7b7aeb3f22ebf2cdaefcc4aac0d64e8deeee8cdc18b9c0f as builder

RUN mkdir -p $GOPATH/pkg/mod $GOPATH/bin $GOPATH/src /build
COPY . /build
WORKDIR /build

RUN apt update && apt install -y tzdata ca-certificates
RUN go mod download

ARG TARGETOS
ARG TARGETARCH
ARG BUILD_FLAGS=""

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} CGO_ENABLED=0 \
    go build ${BUILD_FLAGS} -o /mulery ./cmd/mulery

FROM scratch
COPY --from=builder /mulery /mulery
# Make sure we have an ssl cert chain and timezone data.
COPY --from=builder /etc/ssl /etc/ssl
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

ENV TZ=UTC
EXPOSE 5555
ENTRYPOINT [ "/mulery" ]
