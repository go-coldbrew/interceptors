.PHONY: build test doc lint bench
build:
	go build ./...

test:
	go test -race ./...
	cd examples && go test -race ./...

doc:
	go tool gomarkdoc --exclude-dirs ./examples --output '{{.Dir}}/README.md' ./...

lint:
	go tool golangci-lint run
	go tool govulncheck -scan=module

bench:
	go test -run=^$$ -bench=. -benchmem ./...
