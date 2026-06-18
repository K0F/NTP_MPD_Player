fmt:
	gofumpt -w .

build:
	go build -o mpdplayer main.go

run:
	go run main.go

lint:
	golangci-lint run
