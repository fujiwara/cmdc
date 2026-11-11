.PHONY: clean test

cmdc: go.* *.go
	go build -o $@ ./cmd/cmdc

clean:
	rm -rf cmdc dist/

test:
	go test -v ./...

install:
	go install github.com/fujiwara/cmdc/cmd/cmdc

dist:
	goreleaser build --snapshot --clean
