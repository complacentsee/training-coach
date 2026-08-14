# --- build ---------------------------------------------------------------
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
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
# The one module requirement (muktihari/fit) is test-only, and module-graph
# pruning means this build needs no network for it. Prove the binary stayed
# dependency-free: fail the image if `go version -m` ever lists a dep line.
RUN ! go version -m /out/runningcoach | grep -qE '^[[:space:]]+dep[[:space:]]'

# --- runtime -------------------------------------------------------------
FROM scratch
# Stamped by the Makefile with a hash of the sources. Image IDs are NOT
# preserved by docker save|load across differing Docker versions, so this
# label is what deploy compares to prove the right build actually landed.
ARG SRC_HASH=unknown
LABEL org.opencontainers.image.revision=$SRC_HASH
COPY --from=build /out/runningcoach /runningcoach
# tzdata so -tz works; ~450 KB and worth it for correct "today"
COPY --from=build /usr/local/go/lib/time/zoneinfo.zip /zoneinfo.zip
ENV ZONEINFO=/zoneinfo.zip
ENV ADDR=:8080 DATA_DIR=/data TZ=America/Chicago
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/runningcoach"]
