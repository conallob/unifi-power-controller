FROM golang:1.27.4 AS build
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w \
      -X github.com/conallob/unifi-power-controller/internal/version.Version=${VERSION} \
      -X github.com/conallob/unifi-power-controller/internal/version.Commit=${COMMIT} \
      -X github.com/conallob/unifi-power-controller/internal/version.Date=${DATE}" \
    -o /out/unifi-power-controller ./cmd/unifi-power-controller

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/unifi-power-controller /unifi-power-controller
EXPOSE 8080
ENTRYPOINT ["/unifi-power-controller"]
