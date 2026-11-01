# The same gates as .github/workflows/ci.yml. Where a gate is more than one
# command it is a target here that CI calls, so the two cannot drift apart.

PG_IMAGE := postgres:18-alpine@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5
# One test container per checkout, named and ported from the checkout path,
# so parallel worktrees never share (and wipe) each other's database.
CHECKOUT_ID := $(shell pwd | shasum | cut -c1-8)
TESTPG_NAME := orbit-testpg-$(CHECKOUT_ID)
TESTPG_PORT := $(shell echo $$((40000 + 0x$(CHECKOUT_ID) % 20000)))
export ORBIT_TEST_DATABASE_URL ?= postgres://postgres:postgres@127.0.0.1:$(TESTPG_PORT)/postgres?sslmode=disable

.PHONY: check fmt fmt-check vet lint test prose validate vuln build image testdb-up testdb-down

check: fmt-check vet lint prose validate test ## everything CI gates on

fmt:            ## format the code
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

lint:           ## staticcheck (CI also runs golangci-lint)
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

test:           ## the tests, against ORBIT_TEST_DATABASE_URL (make testdb-up)
	go test -race -count=1 ./...

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
	@echo "ORBIT_TEST_DATABASE_URL=$(ORBIT_TEST_DATABASE_URL)"

testdb-down:
	-docker rm -f $(TESTPG_NAME)
