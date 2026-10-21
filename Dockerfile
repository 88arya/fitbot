FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /fitbot ./cmd/fitbot

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /fitbot /fitbot
EXPOSE 8080
ENTRYPOINT ["/fitbot"]
