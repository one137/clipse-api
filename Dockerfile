FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /clipse-api

FROM alpine:3.22

COPY --from=build /clipse-api /usr/local/bin/clipse-api

# Same uid as dockeruser, which owns the history file
USER 1001:1001

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/clipse-api"]
