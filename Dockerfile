FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/docstore ./cmd/docstore

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /bin/docstore /usr/local/bin/docstore
ENTRYPOINT ["docstore"]
