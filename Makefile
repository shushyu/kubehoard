BINARY := kubehoard

.PHONY: build test vet fmt tidy clean

build:
	go build -o $(BINARY) ./cmd/kubehoard

test:
	go test ./...

vet:
	go vet ./...

fmt:
	@files=$$(gofmt -l .); if [ -n "$$files" ]; then echo "gofmt needed:"; echo "$$files"; exit 1; fi

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)
