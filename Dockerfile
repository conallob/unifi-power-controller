FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/unifi-power-controller ./cmd/unifi-power-controller

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/unifi-power-controller /unifi-power-controller
EXPOSE 8080
ENTRYPOINT ["/unifi-power-controller"]
