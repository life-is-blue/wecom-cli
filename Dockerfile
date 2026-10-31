FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/openetl-gateway ./cmd/openetl-gateway
FROM alpine:3.20
COPY --from=build /out/openetl-gateway /usr/local/bin/openetl-gateway
EXPOSE 8080
ENTRYPOINT ["openetl-gateway"]
CMD ["serve"]
