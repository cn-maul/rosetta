.PHONY: all check fmt vet test race cover staticcheck tidy work-test work-smoke

all: check

fmt:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed on:"; echo "$$unformatted"; exit 1; fi

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race ./...

cover:
	go test ./... -coverprofile=coverage.out
	go tool cover -func=coverage.out

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 -checks=all ./...

tidy:
	go mod tidy -diff

# Work modules under work/ are their own modules: the root `go test ./...`
# deliberately skips them, so a failing repro never reddens CI. Run them
# on purpose.
work-test:
	cd work/anthropic-stopseq-repro && go test ./...

# Live smoke against a real Anthropic-compatible endpoint. Needs
# TIANTOKEN_KEY (and a reachable host in work/anthropic-live-check/main.go).
work-smoke:
	cd work/anthropic-live-check && go run .

check: fmt vet test
