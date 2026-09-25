# Backend: one static binary, no runtime dependencies.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
COPY --from=build /src/data /data
# Containers log to stdout so the platform's log viewer captures them; set
# LOG_OUTPUT=both and mount /logs to also keep rotating files.
# No SERVER_PORT here: hosts such as Render inject PORT, which the server honours
# (default 8080).
ENV HISTORY_FILE=/data/history_1m.json LOG_OUTPUT=stdout LOG_DIR=/logs
EXPOSE 8080
ENTRYPOINT ["/server"]
