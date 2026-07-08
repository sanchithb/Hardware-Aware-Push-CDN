# hpcdn — single image for all three roles (controller | edge | origin).
# Select the role with the container command, e.g.:
#   docker run ghcr.io/you/hpcdn controller
#   docker run ghcr.io/you/hpcdn edge --controller-url http://controller:8080 --join-token hpj_…

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w \
        -X github.com/sanchithb/hardware-aware-push-cdn/pkg/version.Version=${VERSION} \
        -X github.com/sanchithb/hardware-aware-push-cdn/pkg/version.Commit=${COMMIT} \
        -X github.com/sanchithb/hardware-aware-push-cdn/pkg/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -o /out/hpcdn ./cmd/hpcdn

FROM alpine:3.20
RUN adduser -D -u 10001 hpcdn && apk add --no-cache ca-certificates
COPY --from=build /out/hpcdn /usr/local/bin/hpcdn
USER hpcdn
ENV HPCDN_DATA_DIR=/var/lib/hpcdn
VOLUME /var/lib/hpcdn
ENTRYPOINT ["hpcdn"]
CMD ["controller"]
