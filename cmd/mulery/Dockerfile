# Build a go app into a minimal docker image with timezone support and SSL cert chains.
FROM golang:latest@sha256:764d7e0ce1df1e4a1bddc6d1def5f3516fdc045c5fad88e61f67fdbd1857282f AS builder

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
