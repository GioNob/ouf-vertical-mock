FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod *.go ./
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOPROXY=off
RUN go build -trimpath -ldflags="-s -w" -o /out/ouf-vertical-mock .

FROM alpine:3.22
RUN mkdir -p /var/lib/ouf-vertical-mock && chown 10008:10008 /var/lib/ouf-vertical-mock
COPY --from=build /out/ouf-vertical-mock /usr/local/bin/ouf-vertical-mock
COPY examples/domain-lab.json /usr/share/ouf-vertical-mock/examples/domain-lab.json
USER 10008:10008
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/ouf-vertical-mock"]
CMD ["serve"]
