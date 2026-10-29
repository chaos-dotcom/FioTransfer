# Build FioTransfer.app into the project folder (signed ad-hoc so macOS opens it).
# The same targets the Go Makefile has: app, cli, test, clean.

APP := FioTransfer.app

.PHONY: app bundle cli bench test clean shots

app: bundle

bundle:
	scripts/bundle.sh

cli:
	cargo build --release --bin fio
	mkdir -p bin && cp target/release/fio bin/fio

bench:
	cargo build --release --bin fio-bench
	mkdir -p bin && cp target/release/fio-bench bin/fio-bench

test:
	cargo test

clean:
	rm -rf $(APP) target bin
