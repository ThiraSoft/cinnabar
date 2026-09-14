FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w \
      -X github.com/ThiraSoft/cinnabar/internal/version.Version=${VERSION} \
      -X github.com/ThiraSoft/cinnabar/internal/version.Commit=${COMMIT} \
      -X github.com/ThiraSoft/cinnabar/internal/version.BuildDate=${BUILD_DATE}" \
    -o /out/cinnabar ./cmd/cinnabar

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cinnabar /cinnabar
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/cinnabar"]
