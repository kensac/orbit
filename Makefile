# The same gates as .github/workflows/ci.yml. Where a gate is more than one
# command it is a target here that CI calls, so the two cannot drift apart.

PG_IMAGE := postgres:18-alpine@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5
# One test container per checkout, named and ported from the checkout path,
# so parallel worktrees never share (and wipe) each other's database.
CHECKOUT_ID := $(shell pwd | shasum | cut -c1-8)
TESTPG_NAME := orbit-testpg-$(CHECKOUT_ID)
TESTPG_PORT := $(shell echo $$((40000 + 0x$(CHECKOUT_ID) % 20000)))
TESTPG_URL := postgres://postgres:postgres@127.0.0.1:$(TESTPG_PORT)/postgres?sslmode=disable

.PHONY: check check-sandbox fmt fmt-check vet lint test test-sandbox compat prose validate vuln build image testdb-up testdb-down

check: fmt-check vet lint prose validate test ## everything CI gates on

check-sandbox: fmt-check vet lint prose validate test-sandbox ## what a build sandbox can run: no Postgres, no cgo

fmt:            ## format the code
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

lint:           ## staticcheck (CI also runs golangci-lint)
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

# ORBIT_TEST_DATABASE_URL from the environment wins (CI sets it, and fails
# if that server is unreachable). Otherwise the tests use this checkout's
# container when it is running, and database tests skip when it is not.
test:           ## the tests, against ORBIT_TEST_DATABASE_URL or make testdb-up's container
	@url="$${ORBIT_TEST_DATABASE_URL:-}"; \
	if [ -z "$$url" ] && [ "$$(docker inspect -f '{{.State.Running}}' $(TESTPG_NAME) 2>/dev/null)" = true ]; then url='$(TESTPG_URL)'; fi; \
	if [ -z "$$url" ]; then echo "no test database: database tests skip (make testdb-up to run them)"; fi; \
	ORBIT_TEST_DATABASE_URL="$$url" go test -race -count=1 ./...

# The previous release (BASE, default origin/main) keeps working on this
# tree's schema, as during a rollout (deploy/compat.sh). Needs a database
# server, as the tests do.
compat:         ## the previous release on this tree's schema (BASE=<commit>)
	@url="$${ORBIT_TEST_DATABASE_URL:-}"; \
	if [ -z "$$url" ] && [ "$$(docker inspect -f '{{.State.Running}}' $(TESTPG_NAME) 2>/dev/null)" = true ]; then url='$(TESTPG_URL)'; fi; \
	if [ -z "$$url" ]; then echo "compat needs a database server: make testdb-up or ORBIT_TEST_DATABASE_URL"; exit 1; fi; \
	ORBIT_TEST_DATABASE_URL="$$url" deploy/compat.sh $(or $(BASE),origin/main)

test-sandbox:   ## the tests without -race (it needs cgo) and with database tests skipping
	ORBIT_TEST_DATABASE_URL= CGO_ENABLED=0 go test -count=1 ./...

prose:          ## fail on an em dash anywhere in the repository
	@if git grep -InF -e "—" -- . ':!Makefile'; then echo "em dash found: write a comma, a colon, or a new sentence"; exit 1; fi

validate:       ## the example configuration loads
	go run ./cmd/orbit validate examples

vuln:           ## known vulnerabilities in code paths orbit calls
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

build:
	CGO_ENABLED=0 go build -trimpath -o bin/orbit ./cmd/orbit

image:
	docker build -f deploy/Dockerfile --build-arg GIT_SHA=$$(git rev-parse HEAD) -t orbit:dev .

testdb-up:      ## start this checkout's disposable Postgres
	docker run -d --rm --name $(TESTPG_NAME) -p 127.0.0.1:$(TESTPG_PORT):5432 \
		-e POSTGRES_PASSWORD=postgres $(PG_IMAGE) >/dev/null
	@until docker exec $(TESTPG_NAME) pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
	@echo "ORBIT_TEST_DATABASE_URL=$(TESTPG_URL)"

testdb-down:
	-docker rm -f $(TESTPG_NAME)
