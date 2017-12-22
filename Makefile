.PHONY: build clean test dep release

build:
	@mkdir -p bin
	@go build -o bin/prometheus-ecs-sd .

clean:
	@rm -f bin/*

test:
	@go test -v ./...

dep:
	@dep ensure
	@dep prune

release:
	@for platform in darwin linux; do \
		env GOOS=$$platform GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/prometheus-ecs-sd-${VERSION}-$$platform-amd64; \
	done
