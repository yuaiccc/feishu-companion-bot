.PHONY: build test doctor build-ocr

build: build-ocr
	go build -o bot ./cmd/bot

test:
	go test ./...
	go vet ./...

build-ocr:
	@if [ "$$(uname -s)" = "Darwin" ]; then \
		mkdir -p bin; \
		xcrun swiftc -O tools/macos-vision-ocr/main.swift -o bin/macos-vision-ocr; \
	else \
		echo "skip Apple Vision OCR helper: non-macOS"; \
	fi

doctor:
	go run ./cmd/doctor
