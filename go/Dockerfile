# Builds one Go component. Usage: docker build --build-arg CMD=gateway go/
# fetch needs git, so it builds with --target fetch.
FROM golang:1.27 AS build
ARG CMD
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN test -n "${CMD}" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

FROM alpine:3.22 AS fetch
RUN apk add --no-cache git ca-certificates
COPY --from=build /out/app /app
# Same UID as the runner image's node user, so git trusts the shared checkout.
USER 1000:1000
ENTRYPOINT ["/app"]

FROM alpine/helm:3.17.3 AS helm

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
