# --- build ---------------------------------------------------------------
FROM golang:1.25-alpine AS build
# The runtime image is scratch, which has no CA roots at all. Everything the
# binary ever spoke was plain HTTP until the grader called a provider over
# TLS and every request failed "certificate signed by unknown authority".
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
# The module fetch is its own layer so editing a .go file does not re-download.
RUN go mod download
COPY *.go ./
# defaults/ is a generic example block and library, embedded so the container
# runs standalone. The real plan is NOT in the image any more: it lives on the
# /data volume, which is what lets Claude ship a new block without a rebuild.
COPY defaults ./defaults
COPY templates ./templates
COPY static ./static
# Static binary: no libc, so the runtime image can be scratch.
ARG SRC_HASH=dev
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w -X main.buildHash=$SRC_HASH" -o /out/runningcoach .
# The stdlib-only law, amended 14 Aug 2026 into an ALLOWLIST: the import
# pipeline decodes activity FIT (muktihari/fit) and fills the metrics cache
# (modernc.org/sqlite); the other nine lines are those two modules' measured
# transitive closure, nothing more. The build still fails the moment any
# other dep line appears in the binary.
RUN go version -m /out/runningcoach | awk '$1=="dep"{print $2}' | sort > /tmp/deps && \
    printf '%s\n' \
      github.com/dustin/go-humanize \
      github.com/google/uuid \
      github.com/mattn/go-isatty \
      github.com/muktihari/fit \
      github.com/ncruces/go-strftime \
      github.com/remyoudompheng/bigfft \
      golang.org/x/sys \
      modernc.org/libc \
      modernc.org/mathutil \
      modernc.org/memory \
      modernc.org/sqlite \
      | sort > /tmp/allow && \
    ! comm -23 /tmp/deps /tmp/allow | grep .

# --- runtime -------------------------------------------------------------
FROM scratch
# Stamped by the Makefile with a hash of the sources. Image IDs are NOT
# preserved by docker save|load across differing Docker versions, so this
# label is what deploy compares to prove the right build actually landed.
ARG SRC_HASH=unknown
LABEL org.opencontainers.image.revision=$SRC_HASH
COPY --from=build /out/runningcoach /runningcoach
# ~200 KB, and without it the grader cannot reach any provider over HTTPS.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
# tzdata so -tz works; ~450 KB and worth it for correct "today"
COPY --from=build /usr/local/go/lib/time/zoneinfo.zip /zoneinfo.zip
ENV ZONEINFO=/zoneinfo.zip
ENV ADDR=:8080 DATA_DIR=/data TZ=America/Chicago
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/runningcoach"]
