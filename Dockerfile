# Builds the server and all agent binaries in one image.
# The agents get PUBLIC_URL and COMPANY_NAME baked in at build time.
FROM golang:1.24-alpine AS build
ARG PUBLIC_URL
ARG COMPANY_NAME="Company"
RUN test -n "$PUBLIC_URL" || (echo "PUBLIC_URL build arg is required" && exit 1)
WORKDIR /src
COPY go.mod ./
COPY internal ./internal
COPY server ./server
COPY agent ./agent
ENV CGO_ENABLED=0
RUN go vet ./... \
 && go build -trimpath -ldflags "-s -w" -o /out/server ./server \
 && LDF="-s -w -X main.serverURL=${PUBLIC_URL} -X 'main.companyName=${COMPANY_NAME}'" \
 && GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDF -H=windowsgui" -o /out/agents/RemoteAgent-windows-amd64.exe ./agent \
 && GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$LDF" -o /out/agents/RemoteAgent-darwin-arm64 ./agent \
 && GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "$LDF" -o /out/agents/RemoteAgent-darwin-amd64 ./agent

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 10001 app \
 && mkdir -p /data /app/agents && chown app:app /data
COPY --from=build /out/server /app/server
COPY --from=build /out/agents /app/agents
USER app
ENV DATA_DIR=/data AGENTS_DIR=/app/agents LISTEN_ADDR=:8080
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/server"]
