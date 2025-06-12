# Build a go app into a minimal docker image with timezone support and SSL cert chains.
FROM golang:latest@sha256:10c131810f80a4802c49cab0961bbe18a16f4bb2fb99ef16deaa23e4246fc817 as builder

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
