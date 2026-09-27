# Build context: fakeshop/
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fakeshop ./cmd/fakeshop

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fakeshop /fakeshop
EXPOSE 8080
ENTRYPOINT ["/fakeshop"]
