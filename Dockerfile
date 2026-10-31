FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/openetl-server ./cmd/openetl-server
FROM alpine:3.20
COPY --from=build /out/openetl-server /usr/local/bin/openetl-server
EXPOSE 8080
ENTRYPOINT ["openetl-server"]
CMD ["serve"]
