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

# The dev stack: same server, own image tag, container, port and volume.
# deploy-dev/seed-dev/logs-dev never touch the live stack.
DEVDEST ?= ~/running-coach-dev
DEVPORT ?= 9998
# How much of the live activity archive seed-dev copies across, in days by
# device name (names are local-time timestamps, so lexicographic ≥ works).
SEED_DAYS ?= 30

SSH := ssh -o ConnectTimeout=10

# Every .go file that compiles into the binary, deepest first. This was
# `ls *.go`, which does not descend: the day a file lands in a subpackage its
# contents stop reaching SRC_HASH, so editing it would not move the build
# revision, deploy would print "remote already has this build" and skip the
# transfer, and verify would pass while the server ran stale code. Silent
# staleness is the one failure this whole file exists to prevent.
#
# data/ and tools/ are pruned rather than filtered: data/ is the volume (no Go,
# and 1,300+ activity files to walk), and tools/ holds build-time programs the
# Dockerfile never copies, so they are not part of the image. With no
# subpackages present this list is the same 20 files, in the same order, that
# `ls *.go | grep -v _test` produced -- so this commit does not move the hash.
GO_SRC = $(shell find . -path ./data -prune -o -path ./tools -prune -o -name '*_test.go' -prune -o -name '*.go' -print | LC_ALL=C sort)

# The assets compiled into the binary, and the same story one line up: this was
# `defaults/*.json defaults/*/*.json templates/*.html static/*.css static/*.js
# static/icons/*`, none of which descends, while `//go:embed defaults` and
# `//go:embed static` both do. static/fonts/x.woff2 would ship inside the
# binary and leave the build revision untouched. find skips the names
# //go:embed itself ignores -- dotfiles and underscore-prefixed -- which also
# keeps .DS_Store and macOS ._* side-files from rotating the hash.
ASSETS = $(shell find defaults templates static -type f -not -name '.*' -not -name '_*' | LC_ALL=C sort)

# A hash of everything that ends up in the image. Docker image IDs do not
# survive save|load between different Docker versions, so this is stamped in
# as a label and compared instead. Tests are excluded: they do not ship, and a
# test-only edit should not force a redeploy.
SRC_HASH = $(shell cat Dockerfile go.mod go.sum $(GO_SRC) $(ASSETS) | shasum -a 256 | cut -c1-12)

# The plan is no longer in the image, so "is the deploy current?" is now two
# questions. This is the answer to the second one, computed by the same code
# the server uses, so local and live are directly comparable.
DATA_REV = go run . -validate -data ./data 2>&1 | sed -n 's/.*rev=\([0-9a-f]*\).*/\1/p'

.PHONY: help run build image test src-hash check-hash validate check-schema schema new-block deploy push-files push-data verify logs fetch-log clean deploy-dev verify-dev seed-dev logs-dev

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

src-hash:       ## print the build revision this tree hashes to
	@echo $(SRC_HASH)

# SRC_HASH's entire job is to notice that a source file changed, and the cases
# it silently missed were both nested: a .go file in a subpackage, and an asset
# in a subdirectory of static/ or defaults/. Both compile into the binary, so
# prove neither can hide again -- write one, watch the hash move, edit it,
# watch it move again, delete it, watch it come back to where it started.
check-hash:     ## prove SRC_HASH sees nested sources and nested assets
	@set -e; \
	trap 'rm -rf internal/hashprobe static/hashprobe; rmdir internal 2>/dev/null || true' EXIT INT TERM; \
	for probe in internal/hashprobe/probe.go static/hashprobe/probe.css; do \
	  DIR=$${probe%/*}; \
	  case $$probe in *.go) HEAD='package hashprobe';; *) HEAD='/* hashprobe */';; esac; \
	  [ ! -e $$DIR ] || { echo "FAILED: $$DIR exists, refusing to clobber it" >&2; exit 1; }; \
	  BEFORE=$$($(MAKE) --no-print-directory src-hash); \
	  mkdir -p $$DIR && printf '%s\n// one\n' "$$HEAD" > $$probe; \
	  ADDED=$$($(MAKE) --no-print-directory src-hash); \
	  printf '%s\n// two\n' "$$HEAD" > $$probe; \
	  EDITED=$$($(MAKE) --no-print-directory src-hash); \
	  rm -rf $$DIR; rmdir internal 2>/dev/null || true; \
	  AFTER=$$($(MAKE) --no-print-directory src-hash); \
	  [ "$$BEFORE" != "$$ADDED" ] || { \
	    echo "FAILED: adding $$probe left the hash at $$BEFORE" >&2; \
	    echo "        editing it would deploy as 'remote already has this build'" >&2; exit 1; }; \
	  [ "$$ADDED" != "$$EDITED" ] || { \
	    echo "FAILED: editing $$probe left the hash at $$ADDED" >&2; exit 1; }; \
	  [ "$$BEFORE" = "$$AFTER" ] || { \
	    echo "FAILED: hash did not return to $$BEFORE after cleanup, got $$AFTER" >&2; exit 1; }; \
	  echo "src-hash      $$probe counted: $$BEFORE -> $$ADDED -> $$EDITED -> $$BEFORE"; \
	done

# The loader validates meaning; check-schema validates shape. encoding/json
# drops an unknown key without complaint, so "lable" is not an error -- it is a
# session whose label silently went missing. validate runs both.
#
# Both live in _test.go files, which SRC_HASH excludes: nothing here reaches the
# image, and no schema file can become something the container needs to start.
validate: check-schema ## load and check ./data without starting a server
	@go run . -validate -data ./data

# Capture, then filter. `go test | sed` reports sed's exit status, so a real
# --- FAIL printed the failure and still exited 0, and validate -> push-files
# -> push-data -> deploy all proceeded on a drifted schema. The pipe stays for
# the one-line success message; the status no longer goes through it.
check-schema:   ## reject data keys that no struct field would receive
	@out=$$(go test -run 'TestDataFilesHaveNoUnknownFields|TestSchemasMatchTheStructs' .) || { \
	  printf '%s\n' "$$out" >&2; \
	  echo "FAILED: the data or the schema files do not match the Go types" >&2; exit 1; }; \
	printf '%s\n' "$$out" | sed 's/^ok.*/schema        ok/'

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
	EXTRA=""; if [ -f data/grading-notes.md ]; then EXTRA="grading-notes.md"; fi; \
	COPYFILE_DISABLE=1 tar --no-xattrs -czf - -C data athlete.json blocks library $$EXTRA \
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

# The dev stack mirrors deploy's verify-everything discipline but is entirely
# parallel: running-coach:dev, running-coach-dev, $(DEVPORT), $(DEVDEST).
# It ships no plan data — that is seed-dev's job, and dev's data comes from
# live, not from this working tree.
deploy-dev:     ## build the current tree, ship and restart ONLY the dev stack
	@set -e; \
	$(MAKE) --no-print-directory image TAG=dev; \
	WANT=$(SRC_HASH); \
	[ -n "$$WANT" ] || { echo "FAILED: could not hash sources" >&2; exit 1; }; \
	echo "build         $$WANT"; \
	HAVE=$$($(SSH) $(REMOTE) "docker inspect $(IMAGE):dev --format '{{index .Config.Labels \"org.opencontainers.image.revision\"}}' 2>/dev/null" || true); \
	if [ "$$WANT" = "$$HAVE" ]; then \
	  echo "remote        already has this build"; \
	else \
	  echo "transferring  from $$HAVE ..."; \
	  docker save $(IMAGE):dev | gzip | $(SSH) $(REMOTE) 'gunzip | docker load' >/dev/null; \
	  GOT=$$($(SSH) $(REMOTE) "docker inspect $(IMAGE):dev --format '{{index .Config.Labels \"org.opencontainers.image.revision\"}}'"); \
	  [ "$$WANT" = "$$GOT" ] || { \
	    echo "FAILED: dev image on $(REMOTE) is build '$$GOT', expected '$$WANT'" >&2; exit 1; }; \
	  echo "transferred   ok"; \
	fi; \
	$(SSH) $(REMOTE) "mkdir -p $(DEVDEST)/data"; \
	scp -q compose.dev.yml $(REMOTE):$(DEVDEST)/compose.yml; \
	$(SSH) $(REMOTE) "cd $(DEVDEST) && DEV_PORT=$(DEVPORT) docker compose up -d" >/dev/null 2>&1; \
	sleep 2; \
	RUN=$$($(SSH) $(REMOTE) "docker inspect running-coach-dev --format '{{index .Config.Labels \"org.opencontainers.image.revision\"}}' 2>/dev/null" || true); \
	[ "$$WANT" = "$$RUN" ] || { \
	  echo "FAILED: dev container is running build '$$RUN', expected '$$WANT'" >&2; exit 1; }; \
	$(MAKE) --no-print-directory verify-dev

# verify-dev checks the build half only. The data half deliberately isn't
# compared against this working tree: dev's plan is a seeded copy of live's,
# which is allowed to differ from ./data here. The rev is still reported so a
# stale seed is visible.
verify-dev:     ## confirm the dev stack serves this exact build
	@set -e; \
	WANT=$(SRC_HASH); \
	RAW=$$(curl -s -m 10 --fail http://$(HOST):$(DEVPORT)/healthz) \
	  || { echo "FAILED: no response from http://$(HOST):$(DEVPORT)/healthz" >&2; exit 1; }; \
	LIVE=$$(printf '%s' "$$RAW" | sed -n 's/.*"build":"\([^"]*\)".*/\1/p'); \
	[ "$$WANT" = "$$LIVE" ] || { \
	  echo "FAILED: dev build '$$LIVE', local '$$WANT' - serving a stale build" >&2; exit 1; }; \
	CODE=$$(curl -s -m 10 -o /dev/null -w '%{http_code}' http://$(HOST):$(DEVPORT)/); \
	[ "$$CODE" = "200" ] || { echo "FAILED: GET / returned $$CODE" >&2; exit 1; }; \
	DREV=$$(printf '%s' "$$RAW" | sed -n 's/.*"data":"\([^"]*\)".*/\1/p'); \
	echo "dev           http://$(HOST):$(DEVPORT)  build $$LIVE  data $$DREV"

# seed-dev copies live's plan and log into the dev stack's volume, plus a
# partial activity slice: the last $(SEED_DAYS) days by device name, and any
# names listed one-per-line in data/seed-curated.txt (gitignored) — the
# curated edge-case set. Plan files copy on the filesystem; activities go
# through the two apps' own APIs (GET live → POST dev, both on localhost)
# because the live archive's files are container-written 0600 and the ssh
# user cannot read them — and dev's postActivity validating each byte it
# takes is a feature, not a workaround. 409 means already seeded, so
# re-seeding is idempotent. blocks/ and library/ are replaced wholesale,
# same as push-files. entries.jsonl is REPLACED on every seed: dev's log is
# disposable by design, and re-seeding is the reset.
seed-dev:       ## populate the dev stack's data from live (partial activities)
	@set -e; \
	CURATED=""; \
	if [ -f data/seed-curated.txt ]; then \
	  CURATED=$$(grep -hE '^[A-Za-z0-9][A-Za-z0-9._-]*\.fit$$' data/seed-curated.txt | tr '\n' ' '); \
	fi; \
	$(SSH) $(REMOTE) "set -e; \
	  SRC=$(DEST)/data; DST=$(DEVDEST)/data; \
	  [ -f \$$SRC/athlete.json ] || { echo 'FAILED: no live data at '\$$SRC >&2; exit 1; }; \
	  mkdir -p \$$DST; \
	  rm -rf \$$DST/blocks \$$DST/library; \
	  cp \$$SRC/athlete.json \$$DST/athlete.json; \
	  cp -R \$$SRC/blocks \$$DST/blocks; \
	  cp -R \$$SRC/library \$$DST/library; \
	  cp \$$SRC/entries.jsonl \$$DST/entries.jsonl 2>/dev/null || true; \
	  cp \$$SRC/grading-notes.md \$$DST/grading-notes.md 2>/dev/null || true; \
	  CUT=\$$(date -d '$(SEED_DAYS) days ago' +%Y-%m-%d); \
	  LISTING=\$$(curl -s -m 30 --fail http://localhost:$(PORT)/api/activities) \
	    || { echo 'FAILED: live /api/activities unreachable — nothing seeded' >&2; exit 1; }; \
	  NAMES=\$$(printf '%s' \"\$$LISTING\" | grep -o '\"name\":\"[^\"]*\"' | cut -d'\"' -f4); \
	  N=0; F=0; T=\$$(mktemp); \
	  for b in \$$NAMES $$CURATED; do \
	    case \$$b in *.fit) ;; *) continue;; esac; \
	    keep=no; \
	    [ \"\$$b\" \\> \"\$$CUT\" ] && keep=yes; \
	    for c in $$CURATED; do [ \"\$$b\" = \"\$$c\" ] && keep=yes; done; \
	    [ \$$keep = yes ] || continue; \
	    CODE=\$$(curl -s -m 30 -o \$$T -w '%{http_code}' \"http://localhost:$(PORT)/api/activity?name=\$$b\"); \
	    [ \"\$$CODE\" = 200 ] || { echo \"WARN: live GET \$$b -> \$$CODE\" >&2; F=\$$((F+1)); continue; }; \
	    CODE=\$$(curl -s -m 30 -o /dev/null -w '%{http_code}' -X POST --data-binary @\$$T \"http://localhost:$(DEVPORT)/api/activity?name=\$$b\"); \
	    case \$$CODE in 204) N=\$$((N+1));; 409) ;; *) echo \"WARN: dev POST \$$b -> \$$CODE\" >&2; F=\$$((F+1));; esac; \
	  done; \
	  rm -f \$$T; \
	  [ \$$F = 0 ] || { echo \"FAILED: \$$F activities did not seed\" >&2; exit 1; }; \
	  echo \"seeded        \$$N new activities this run\""; \
	curl -s -m 10 --fail -X POST http://$(HOST):$(DEVPORT)/api/reload >/dev/null \
	  || { echo "FAILED: dev /api/reload rejected the seeded plan" >&2; exit 1; }; \
	echo "seed          done — dev serving the seeded plan"

logs-dev:       ## tail the dev container log
	$(SSH) $(REMOTE) 'docker logs -f running-coach-dev'

fetch-log:      ## pull the entry log back for Claude to read
	scp $(REMOTE):$(DEST)/data/entries.jsonl ./data/entries.jsonl

clean:          ## remove the local binary
	rm -f $(BIN)
