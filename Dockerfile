# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM node:22-alpine@sha256:16e22a550f3863206a3f701448c45f7912c6896a62de43add43bb9c86130c3e2 AS frontend
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY frontend/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.25.5-alpine@sha256:ac09a5f469f307e5da71e766b0bd59c9c49ea460a528cc3e6686513d64a6f1fb AS backend
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    for attempt in 1 2 3; do go mod download && exit 0; sleep $((attempt * 2)); done; exit 1
COPY . ./
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X dbgold/api/handler.Version=${VERSION} -X dbgold/api/handler.GitCommit=${GIT_COMMIT} -X dbgold/api/handler.BuildTime=${BUILD_TIME}" \
    -o /out/dbgold .

FROM gcr.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35
WORKDIR /app
COPY --from=backend /out/dbgold /app/dbgold
COPY --from=frontend /src/frontend/dist /app/web
ENV APP_ENV=production \
    LISTEN_HOST=0.0.0.0 \
    PORT=18089 \
    STATIC_DIR=/app/web \
    SQLITE_PATH=/app/data/dbgold.db \
    UPLOAD_DIR=/app/uploads \
    LOG_DIR=/app/logs
USER nonroot:nonroot
EXPOSE 18089
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/app/dbgold", "healthcheck"]
ENTRYPOINT ["/app/dbgold"]
CMD ["serve"]
