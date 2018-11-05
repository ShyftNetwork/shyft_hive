FROM golang:1.10.3-alpine

# To copy complete directory
COPY ./ /go/src/github.com/ShyftNetwork/go-empyrean
WORKDIR /go/src/github.com/ShyftNetwork/go-empyrean

RUN \
  apk add --update git make gcc musl-dev linux-headers ca-certificates && \
  (cd /go/src/github.com/ShyftNetwork/go-empyrean && make geth && make bootnode) && \
  cp -v /go/src/github.com/ShyftNetwork/go-empyrean/build/bin/geth /bin && \
  cp -v /go/src/github.com/ShyftNetwork/go-empyrean/build/bin/bootnode /bin

WORKDIR /go/src/github.com/ShyftNetwork/go-empyrean

ENTRYPOINT ["/geth.sh"]