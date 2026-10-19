# Build FioTransfer.app into the project folder (signed ad-hoc so macOS opens it).
APP := FioTransfer.app

.PHONY: app dmg cli test clean
app:
	~/go/bin/wails build -platform darwin/arm64 -clean -trimpath
	rm -rf $(APP)
	cp -R build/bin/$(APP) $(APP)
	cp LICENSE $(APP)/Contents/Resources/LICENSE.txt
	cp THIRD_PARTY_NOTICES.txt $(APP)/Contents/Resources/
	codesign --force --deep -s - $(APP)
	@echo "Built ./$(APP)"

# Universal .dmg + .zip in dist/ (same script CI uses). VERSION=1.2.3 make dmg
dmg:
	scripts/package.sh $(or $(VERSION),dev)

cli:
	go build -o bin/fio ./cmd/fio

test:
	go test -race ./internal/...

clean:
	rm -rf $(APP) build/bin bin dist
