# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM node:22-alpine@sha256:16e22a550f3863206a3f701448c45f7912c6896a62de43add43bb9c86130c3e2 AS frontend
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --audit=false
COPY frontend/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.25.5-alpine@sha256:ac09a5f469f307e5da71e766b0bd59c9c49ea460a528cc3e6686513d64a6f1fb AS backend
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
ARG GOPROXY=https://proxy.golang.org,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    for attempt in 1 2 3; do \
      echo "Downloading Go modules (attempt ${attempt}/3, timeout 180s, GOPROXY=${GOPROXY})"; \
      if GOPROXY="${GOPROXY}" timeout 180s go mod download; then exit 0; fi; \
      if [ "$attempt" -lt 3 ]; then sleep $((attempt * 2)); fi; \
    done; \
    echo "Failed to download Go modules after 3 attempts." >&2; \
    exit 1
COPY . ./
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X dbgold/api/handler.Version=${VERSION} -X dbgold/api/handler.GitCommit=${GIT_COMMIT} -X dbgold/api/handler.BuildTime=${BUILD_TIME}" \
    -o /out/dbgold .

FROM --platform=$BUILDPLATFORM eclipse-temurin:17-jdk-jammy AS jdbc-helper
WORKDIR /src
COPY jdbcbridge/java/src/ ./src/
RUN mkdir -p /out/classes && \
    javac --release 8 -encoding UTF-8 -d /out/classes src/com/dbgold/oscar/BridgeMain.java && \
    jar --create --file /out/dbgold-oscar-bridge.jar --main-class com.dbgold.oscar.BridgeMain -C /out/classes .

FROM eclipse-temurin:17-jre-jammy
WORKDIR /app
RUN groupadd --system dbgold && useradd --system --gid dbgold --home-dir /app --no-create-home dbgold && \
    mkdir -p /app/data /app/uploads /app/logs /app/lib && chown -R dbgold:dbgold /app
COPY --from=backend /out/dbgold /app/dbgold
COPY --from=frontend /src/frontend/dist /app/web
COPY --from=jdbc-helper /out/dbgold-oscar-bridge.jar /app/lib/dbgold-oscar-bridge.jar
COPY third_party/oscar/oscarJDBC8.jar /app/lib/oscarJDBC8.jar
COPY third_party/oscar/README.md /app/lib/OSCAR-JDBC-NOTICE.md
COPY third_party/oscar/LICENSE /app/lib/OSCAR-JDBC-LICENSE
ENV APP_ENV=production \
    LISTEN_HOST=0.0.0.0 \
    PORT=18089 \
    STATIC_DIR=/app/web \
    SQLITE_PATH=/app/data/dbgold.db \
    UPLOAD_DIR=/app/uploads \
    LOG_DIR=/app/logs
RUN java -version && java -cp /app/lib/dbgold-oscar-bridge.jar:/app/lib/oscarJDBC8.jar com.dbgold.oscar.BridgeMain </dev/null
USER dbgold:dbgold
EXPOSE 18089
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/app/dbgold", "healthcheck"]
ENTRYPOINT ["/app/dbgold"]
CMD ["serve"]
