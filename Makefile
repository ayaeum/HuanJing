.PHONY: build run test demo load clean
build:
	go build -trimpath -o bin/huanjing ./cmd/server
run:
	go run ./cmd/server
test:
	go test -race ./...
demo:
	python3 scripts/demo.py
load:
	python3 scripts/loadtest.py --help
clean:
	rm -rf bin
