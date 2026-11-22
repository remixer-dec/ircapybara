.PHONY: test vet build docker

test:
	go test ./...

vet:
	go vet ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ircapybara ./cmd/ircapybara

docker:
	docker compose build ircapybara
