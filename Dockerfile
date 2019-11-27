FROM --platform=$TARGETPLATFORM moikot/golang-dep as build-env

ARG TRAVIS_REPO_SLUG=dummy/main

LABEL maintainer="gareth.dunstone@anu.edu.au"

ENV APP_FOLDER=/go/src/github.com/$TRAVIS_REPO_SLUG

ADD . ${APP_FOLDER}
WORKDIR ${APP_FOLDER}

RUN dep ensure -vendor-only

# Compile independent executable
RUN CGO_ENABLED=0 GOOS=linux go build -v -a -o /bin/main .

FROM --platform=$TARGETPLATFORM alpine:latest

RUN apk add --no-cache tzdata

COPY --from=build-env /bin/main /

ENTRYPOINT ["/main"]