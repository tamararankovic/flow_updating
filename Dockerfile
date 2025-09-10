FROM golang:latest AS builder

WORKDIR /app

COPY flow_updating/go.mod flow_updating/go.sum ./

COPY hyparview ../hyparview

RUN go mod download

COPY flow_updating .

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /main .

FROM alpine:latest

WORKDIR /app

RUN mkdir -p /var/log/fu

# Copy Go binaries
COPY --from=builder  /main  ./main

CMD [ "/app/main" ]