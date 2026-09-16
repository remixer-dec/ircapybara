.PHONY: test vet build docker

test:
	go test ./...

vet:
	go vet ./...

build:
	CGO_ENABLED=0 go build -trimpath -o ircapybara ./cmd/ircapybara

docker:
	docker compose build ircapybara
