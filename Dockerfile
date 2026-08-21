FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/billett ./cmd/billett

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata \
	&& addgroup -S billett \
	&& adduser -S -G billett billett \
	&& mkdir -p /data \
	&& chown billett:billett /data
COPY --from=build /out/billett /usr/local/bin/billett

USER billett
VOLUME ["/data"]
ENV BILLETT_ADDR=0.0.0.0:8080
ENV BILLETT_DB=/data/billett.sqlite
EXPOSE 8080
STOPSIGNAL SIGTERM

ENTRYPOINT ["billett"]
CMD ["serve"]
