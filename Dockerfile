FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bookclub ./cmd/bookclub \
 && mkdir -p /out/data

# distroless/static has CA certificates (needed for api.telegram.org) and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bookclub /bookclub
# A named volume mounted here inherits this ownership, so the non-root user can write to it.
COPY --from=build --chown=65532:65532 /out/data /data
ENV ADDR=:8080 DATA_FILE=/data/bookclub.json
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/bookclub"]
