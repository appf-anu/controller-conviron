FROM --platform $BUILDPLATFORM moikot/golang-dep as build-env

ARG APP_FOLDER=/go/src/github.com/appf/controller-conviron/

ADD . ${APP_FOLDER}
WORKDIR ${APP_FOLDER}

RUN dep ensure -vendor-only

# Compile independent executable
RUN CGO_ENABLED=0 GOOS=linux go build -a -o /bin/main .

FROM --platform=$TARGETPLATFORM alpine:latest

RUN apk add --no-cache tzdata

COPY --from=build-env /bin/main /

ENTRYPOINT ["/main"]