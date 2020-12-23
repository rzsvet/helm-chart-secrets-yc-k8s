FROM golang:1.15-alpine AS build

# Installing requirements
RUN apk add --update git && \
    rm -rf /tmp/* /var/tmp/* /var/cache/apk/* /var/cache/distfiles/*

# Creating workdir and copying dependencies
WORKDIR /go/src/app
COPY . .

# Installing dependencies
RUN go get
ENV CGO_ENABLED=0

RUN go build -o api main.go requests.go

FROM alpine:3.9.6

RUN echo "http://dl-cdn.alpinelinux.org/alpine/edge/testing/" >> /etc/apk/repositories && \
    apk add --update bash && \
    rm -rf /tmp/* /var/tmp/* /var/cache/apk/* /var/cache/distfiles/*

WORKDIR /app

COPY --from=build /go/src/app/api /app/api
COPY ./migrations/ /app/migrations/

CMD ["/app/api"]
