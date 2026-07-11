# ── Build stage ─────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/saathi-server ./cmd/server \
 && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/saathi-seed ./cmd/seed

# ── Runtime stage ───────────────────────────────────────────────────────────
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S saathi && adduser -S saathi -G saathi
USER saathi
COPY --from=build /out/saathi-server /usr/local/bin/saathi-server
COPY --from=build /out/saathi-seed /usr/local/bin/saathi-seed
EXPOSE 8080
ENTRYPOINT ["saathi-server"]
