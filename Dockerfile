FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates

COPY go.mod ./
COPY . .

ARG BUILDPLATFORM
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/unhealthy .

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/unhealthy /unhealthy

ENTRYPOINT ["/unhealthy"]
