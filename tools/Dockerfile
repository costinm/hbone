
#FROM golang:alpine AS build
#
#RUN env
#
#WORKDIR /ws
#
#ENV GO111MODULE=on
#ENV CGO_ENABLED=0
#ENV GOOS=linux
#ENV GOPROXY=https://proxy.golang.org
#
##COPY go.mod go.sum ./
##COPY cmd/hbone/ ./cmd/hbone/
##RUN pwd && ls -R && cd cmd/hbone && go mod download
#
#COPY .. .
#
## Runs in /go directory
#RUN pwd && ls -R && cd cmd/hbone && go build -a -gcflags='all=-N -l' -ldflags '-extldflags "-static"' \
#  -o ../../hbone ./

FROM alpine:latest

#COPY --from=build /ws/hbone /hbone
COPY hbone /hbone


EXPOSE 15007
EXPOSE 8081
EXPOSE 8080
EXPOSE 15009
EXPOSE 15003

ENTRYPOINT /hbone
