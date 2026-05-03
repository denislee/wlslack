.PHONY: build run clean tidy

build:
	go build -o wlslack ./cmd/wlslack

run: build
	./wlslack

tidy:
	go mod tidy

clean:
	rm -f wlslack
