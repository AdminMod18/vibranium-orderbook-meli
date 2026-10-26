# --- build ---
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/loadgen ./cmd/loadgen

# --- runtime (mínimo; alpine solo para tener wget en el healthcheck) ---
FROM alpine:3.20
RUN adduser -D -u 10001 app && mkdir /data && chown app /data
USER app
COPY --from=build /out/server /server
COPY --from=build /out/loadgen /loadgen
ENV PORT=8080 JOURNAL_PATH=/data/journal.log
EXPOSE 8080
ENTRYPOINT ["/server"]
