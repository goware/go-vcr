build:
	go build ./...

get:
	go get -v -t -d ./...

test:
	go test -v -race ./...


.PHONY: get test
