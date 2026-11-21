FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod *.go ./
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOPROXY=off
RUN go build -trimpath -ldflags="-s -w" -o /out/vertical . && mkdir -p /out/data

FROM scratch
COPY --from=build /out/vertical /vertical
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENTRYPOINT ["/vertical"]
