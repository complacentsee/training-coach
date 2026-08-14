BIN     := runningcoach
IMAGE   := running-coach
TAG     ?= latest
REF     := $(IMAGE):$(TAG)

# Deployment coordinates. The repo carries placeholders; put the real ones in
# local.mk (gitignored) beside this Makefile. Use an ~/.ssh/config alias for
# REMOTE that pins a dedicated key (IdentityAgent none) — an agent that wants
# interactive approval makes non-interactive deploys fail with "too many
# authentication failures".
-include local.mk
HOST    ?= your-server
REMOTE  ?= your-server
DEST    ?= ~/running-coach
PORT    ?= 9999

SSH := ssh -o ConnectTimeout=10

# A hash of everything that ends up in the image. Docker image IDs do not
# survive save|load between different Docker versions, so this is stamped in
# as a label and compared instead. Tests are excluded: they do not ship, and a
# test-only edit should not force a redeploy.
SRC_HASH = $(shell cat Dockerfile go.mod go.sum `ls *.go | grep -v _test` defaults/*.json defaults/*/*.json templates/*.html static/*.css static/*.js static/icons/* 2>/dev/null | shasum -a 256 | cut -c1-12)

# The plan is no longer in the image, so "is the deploy current?" is now two
# questions. This is the answer to the second one, computed by the same code
# the server uses, so local and live are directly comparable.
DATA_REV = go run . -validate -data ./data 2>&1 | sed -n 's/.*rev=\([0-9a-f]*\).*/\1/p'

.PHONY: help run build image test validate check-schema schema new-block deploy push-files push-data verify logs fetch-log clean

help:           ## list targets
	@grep -hE '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*## /|/' | column -t -s'|'

run:            ## run locally on :8080 against ./data
	go run . -addr :8080 -data ./data

build:          ## build a local binary
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) .

image:          ## build the linux/arm64 image
	docker buildx build --platform linux/arm64 --build-arg SRC_HASH=$(SRC_HASH) -t $(REF) --load .

test:           ## run the test suite
	go test ./...

# The loader validates meaning; check-schema validates shape. encoding/json
# drops an unknown key without complaint, so "lable" is not an error -- it is a
# session whose label silently went missing. validate runs both.
#
# Both live in _test.go files, which SRC_HASH excludes: nothing here reaches the
# image, and no schema file can become something the container needs to start.
validate: check-schema ## load and check ./data without starting a server
	@go run . -validate -data ./data

check-schema:   ## reject data keys that no struct field would receive
	@go test -run 'TestDataFilesHaveNoUnknownFields|TestSchemasMatchTheStructs' . \
	  | sed 's/^ok.*/schema        ok/'

schema:         ## rewrite app/schema/*.json after changing a struct
	@go test -run TestSchemasMatchTheStructs -update-schema . >/dev/null && \
	  echo "schema        rewritten from the Go types"

# Blocks start on a Monday, so the scaffold works that out rather than letting
# you discover it from a load failure. WEEKS defaults to 16; a 4- or 8-week
# block is WEEKS=8 make new-block.
new-block:      ## scaffold a block: make new-block ID=2027-spring-5k START=2027-03-01 [WEEKS=16]
	@set -e; \
	[ -n "$(ID)" ] || { echo "usage: make new-block ID=<id> START=<yyyy-mm-dd> [WEEKS=16]" >&2; exit 1; }; \
	[ -n "$(START)" ] || { echo "usage: make new-block ID=<id> START=<yyyy-mm-dd> [WEEKS=16]" >&2; exit 1; }; \
	[ -e data/blocks/$(ID).json ] && { echo "FAILED: data/blocks/$(ID).json exists" >&2; exit 1; }; \
	go run ./tools/newblock -id '$(ID)' -start '$(START)' -weeks '$(or $(WEEKS),16)' \
	  > data/blocks/$(ID).json.tmp; \
	mv data/blocks/$(ID).json.tmp data/blocks/$(ID).json; \
	echo "wrote         data/blocks/$(ID).json"; \
	$(MAKE) --no-print-directory validate

# These ship the plan, which no longer travels inside the image. They never
# touch entries.jsonl: that is his health log, it only exists on the server, and
# nothing here is allowed to overwrite it. blocks/ and library/ are replaced
# wholesale so a renamed block does not leave its old self behind.
#
# deploy uses push-files, not push-data, and does it BEFORE restarting the
# container. Restarting first would leave the new binary briefly serving the
# embedded example block off an empty volume.
# COPYFILE_DISABLE stops macOS bsdtar emitting AppleDouble "._name.json"
# side-files for anything carrying an xattr. Those extracted onto the server as
# binary files with a .json name, the loader tried to parse them, and the
# container crash-looped. The loader now ignores dotfiles too -- belt and
# braces, because this junk arrives from several directions.
push-files: validate ## ship athlete/blocks/library to the server WITHOUT reloading
	@set -e; \
	$(SSH) $(REMOTE) "mkdir -p $(DEST)/data && rm -rf $(DEST)/data/blocks $(DEST)/data/library"; \
	COPYFILE_DISABLE=1 tar --no-xattrs -czf - -C data athlete.json blocks library \
	  | $(SSH) $(REMOTE) "tar -xzf - -C $(DEST)/data"; \
	JUNK=$$($(SSH) $(REMOTE) "find $(DEST)/data -name '._*' -o -name '.DS_Store' | wc -l | tr -d ' '"); \
	[ "$$JUNK" = "0" ] || { \
	  echo "FAILED: $$JUNK junk file(s) landed in $(DEST)/data" >&2; exit 1; }; \
	echo "plan          shipped"

push-data: push-files ## ship the plan and reload it in place, no restart
	@set -e; \
	WANT=$$($(DATA_REV)); \
	OUT=$$(curl -s -m 10 --fail -X POST http://$(HOST):$(PORT)/api/reload) \
	  || { echo "FAILED: /api/reload rejected the new data" >&2; \
	       echo "        the server is still serving the previous plan - check 'make logs'" >&2; exit 1; }; \
	LIVE=$$(printf '%s' "$$OUT" | sed -n 's/.*"now":"\([^"]*\)".*/\1/p'); \
	[ "$$WANT" = "$$LIVE" ] || { \
	  echo "FAILED: live data rev '$$LIVE', local '$$WANT'" >&2; exit 1; }; \
	echo "data          rev $$LIVE live"

# deploy verifies rather than assumes. `docker compose up -d` exits 0 and
# prints something reassuring whether or not a new image ever arrived, and a
# dead ssh mid-pipe looks identical to success. That cost three wrong
# conclusions before this existed, so every step that can fail silently is
# now checked against the server.
deploy: image   ## ship over ssh, restart, and prove it landed
	@set -e; \
	WANT=$(SRC_HASH); \
	[ -n "$$WANT" ] || { echo "FAILED: could not hash sources" >&2; exit 1; }; \
	echo "build         $$WANT"; \
	HAVE=$$($(SSH) $(REMOTE) "docker inspect $(REF) --format '{{index .Config.Labels \"org.opencontainers.image.revision\"}}' 2>/dev/null" || true); \
	if [ "$$WANT" = "$$HAVE" ]; then \
	  echo "remote        already has this build"; \
	else \
	  echo "transferring  from $$HAVE ..."; \
	  docker save $(REF) | gzip | $(SSH) $(REMOTE) 'gunzip | docker load' >/dev/null; \
	  GOT=$$($(SSH) $(REMOTE) "docker inspect $(REF) --format '{{index .Config.Labels \"org.opencontainers.image.revision\"}}'"); \
	  [ "$$WANT" = "$$GOT" ] || { \
	    echo "FAILED: image on $(REMOTE) is build '$$GOT', expected '$$WANT'" >&2; \
	    echo "        the transfer did not land - check ssh auth" >&2; exit 1; }; \
	  echo "transferred   ok"; \
	fi; \
	$(SSH) $(REMOTE) "mkdir -p $(DEST)"; \
	scp -q compose.yml $(REMOTE):$(DEST)/compose.yml; \
	$(MAKE) --no-print-directory push-files; \
	$(SSH) $(REMOTE) "cd $(DEST) && docker compose up -d" >/dev/null 2>&1; \
	sleep 2; \
	RUN=$$($(SSH) $(REMOTE) "docker inspect running-coach --format '{{index .Config.Labels \"org.opencontainers.image.revision\"}}' 2>/dev/null" || true); \
	[ "$$WANT" = "$$RUN" ] || { \
	  echo "FAILED: container is running build '$$RUN', expected '$$WANT'" >&2; exit 1; }; \
	$(MAKE) --no-print-directory verify

# Two independent things can now be stale -- the binary and the plan -- so
# verify checks both. Without the data half, an empty volume would quietly
# serve the embedded example block and every check here would still pass.
verify:         ## confirm the live site serves this exact build AND this exact plan
	@set -e; \
	WANT=$(SRC_HASH); \
	RAW=$$(curl -s -m 10 --fail http://$(HOST):$(PORT)/healthz) \
	  || { echo "FAILED: no response from http://$(HOST):$(PORT)/healthz" >&2; exit 1; }; \
	LIVE=$$(printf '%s' "$$RAW" | sed -n 's/.*"build":"\([^"]*\)".*/\1/p'); \
	[ -n "$$LIVE" ] || { \
	  echo "FAILED: live build predates build reporting (healthz said: $$RAW)" >&2; \
	  echo "        that itself means it is stale - deploy" >&2; exit 1; }; \
	[ "$$WANT" = "$$LIVE" ] || { \
	  echo "FAILED: live build '$$LIVE', local '$$WANT' - serving a stale build" >&2; exit 1; }; \
	WANTD=$$($(DATA_REV)); \
	LIVED=$$(printf '%s' "$$RAW" | sed -n 's/.*"data":"\([^"]*\)".*/\1/p'); \
	[ "$$WANTD" = "$$LIVED" ] || { \
	  echo "FAILED: live plan rev '$$LIVED', local '$$WANTD'" >&2; \
	  echo "        the server is serving different data - run 'make push-data'" >&2; exit 1; }; \
	CODE=$$(curl -s -m 10 -o /dev/null -w '%{http_code}' http://$(HOST):$(PORT)/); \
	[ "$$CODE" = "200" ] || { echo "FAILED: GET / returned $$CODE" >&2; exit 1; }; \
	FITZ=$$(curl -s -m 10 -o /tmp/rc-verify-fit.zip -w '%{http_code}' http://$(HOST):$(PORT)/fit.zip); \
	if [ "$$FITZ" = "200" ]; then \
	  MAGIC=$$(dd if=/tmp/rc-verify-fit.zip bs=1 count=2 2>/dev/null); \
	  [ -s /tmp/rc-verify-fit.zip ] && [ "$$MAGIC" = "PK" ] || { \
	    echo "FAILED: /fit.zip served junk (not a zip)" >&2; exit 1; }; \
	elif [ "$$FITZ" != "404" ]; then \
	  echo "FAILED: /fit.zip returned $$FITZ (want 200 with steps days, 404 without)" >&2; exit 1; \
	fi; \
	ENTRIES=$$(curl -s -m 10 http://$(HOST):$(PORT)/api/entries | grep -o '"ts"' | wc -l | tr -d ' '); \
	echo "deployed      http://$(HOST):$(PORT)  build $$LIVE  $$ENTRIES log entries intact"

logs:           ## tail the container log
	$(SSH) $(REMOTE) 'docker logs -f running-coach'

fetch-log:      ## pull the entry log back for Claude to read
	scp $(REMOTE):$(DEST)/data/entries.jsonl ./data/entries.jsonl

clean:          ## remove the local binary
	rm -f $(BIN)
