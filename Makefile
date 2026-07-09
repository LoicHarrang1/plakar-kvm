GO=go
EXT=

all: build

build:
	${GO} build -v -o kvmImporter${EXT} ./plugin/importer
	${GO} build -v -o kvmExporter${EXT} ./plugin/exporter

test:
	${GO} test ./...

clean:
	rm -f kvmImporter kvmExporter kvm-*.ptar
