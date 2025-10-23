FROM golang:1.24.9-alpine as builder

# Force Go to use the cgo based DNS resolver. This is required to ensure DNS
# queries required to connect to linked containers succeed.
ENV GODEBUG netdns=cgo

# Pass a tag, branch or a commit using build-arg. This allows a docker image to
# be built from a specified Git state. The default image will use the Git tip of
# master by default.
ARG checkout="master"
ARG git_url="https://github.com/guggero/block-dn"

# Install dependencies and build the binaries.
RUN apk add --no-cache --update alpine-sdk \
    git \
    make \
&&  git clone $git_url /go/src/github.com/guggero/block-dn \
&&  cd /go/src/github.com/guggero/block-dn \
&&  git checkout $checkout \
&&  make install

# Start a new, final image.
FROM alpine as final

# Define a root volume for data persistence.
VOLUME /root/.block-dn

# Add utilities for quality of life and SSL-related reasons. We also require
# curl and gpg for the signature verification script.
RUN apk --no-cache add \
    bash \
    jq \
    ca-certificates \
    gnupg \
    curl

# Copy the binaries from the builder image.
COPY --from=builder /go/bin/block-dn /bin/

EXPOSE 8080

# Specify the start command and entrypoint as the block-dn daemon.
ENTRYPOINT ["block-dn"]