FROM alpine:latest

RUN apk add --no-cache tzdata

COPY controller-conviron /bin

ENTRYPOINT ["controller-conviron"]
