.PHONY: all check fmt vet test race cover staticcheck tidy

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

check: fmt vet test
