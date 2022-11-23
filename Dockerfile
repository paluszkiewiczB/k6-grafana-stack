FROM golang:1.19-alpine3.16 as builder
WORKDIR /go/src/app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build

FROM scratch
WORKDIR app
COPY --from=builder /go/src/app/k6gpt main
ENTRYPOINT ["./main"]