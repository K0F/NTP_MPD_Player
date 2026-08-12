fmt:
	gofumpt -w .

build:
	go build -o mpdplayer main.go

run:
	go run main.go

test:
	go test ./...

lint:
	golangci-lint run
