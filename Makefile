.PHONY: build test doc
build:
	go build ./...

test:
	go test ./...

doc:
	go install github.com/princjef/gomarkdoc/cmd/gomarkdoc
	gomarkdoc ./...
