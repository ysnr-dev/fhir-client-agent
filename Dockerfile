FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /fhir-client-agent .

# 送れなかったイベントの退避先。nonroot が書けるよう、所有者を合わせて作っておく。
RUN mkdir -p /spool

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /fhir-client-agent /usr/local/bin/fhir-client-agent
COPY --from=build --chown=nonroot:nonroot /spool /spool
ENV SPOOL_DIR=/spool
ENTRYPOINT ["/usr/local/bin/fhir-client-agent"]
