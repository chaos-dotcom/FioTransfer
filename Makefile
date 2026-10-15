# Build FioTransfer.app into the project folder (signed ad-hoc so macOS opens it).
APP := FioTransfer.app

.PHONY: app cli test clean
app:
	~/go/bin/wails build -platform darwin/arm64 -clean -trimpath
	rm -rf $(APP)
	cp -R build/bin/$(APP) $(APP)
	codesign --force --deep -s - $(APP)
	@echo "Built ./$(APP)"

cli:
	go build -o bin/fio ./cmd/fio

test:
	go test -race ./internal/...

clean:
	rm -rf $(APP) build/bin bin
