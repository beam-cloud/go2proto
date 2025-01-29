build:
	go build -o bin/go2proto go2proto.go && sudo cp bin/go2proto /usr/local/bin/go2proto

test:
	go test -v ./...
