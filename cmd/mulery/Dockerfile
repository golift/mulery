# Build a go app into a minimal docker image with timezone support and SSL cert chains.
FROM golang:latest@sha256:bc45dfd319e982dffe4de14428c77defe5b938e29d9bc6edfbc0b9a1fc171cb3 AS builder

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
