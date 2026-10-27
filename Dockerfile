# The binary is built on the host first (deploy/yandex-cloud.sh does this):
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/bookclub ./cmd/bookclub
# so building the image pulls nothing from Docker Hub, which is often unreachable from Russia.
# distroless/static has the CA certificates needed for api.telegram.org and Object Storage.
FROM gcr.io/distroless/static-debian12:nonroot
COPY dist/bookclub /bookclub
# File mode (no DATA_BUCKET) keeps data in /home/nonroot/data; mount a volume there.
WORKDIR /home/nonroot
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/bookclub"]
