# Builds one Go component. Usage: docker build --build-arg CMD=gateway go/
FROM golang:1.27 AS build
ARG CMD
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN test -n "${CMD}" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
