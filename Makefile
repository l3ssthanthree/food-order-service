.PHONY: test vet fmt-check build up down logs smoke

test:
	go test -p 1 -race ./...

vet:
	go vet ./...

fmt-check:
	test -z "$$(gofmt -l cmd internal migrations)"

build:
	go build ./...

up:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f kitchen demo-restaurant

smoke:
	go run ./cmd/smoke
