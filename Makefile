GO=go
EXT=

all: build

build:
	${GO} build -v -o kvmImporter${EXT} ./plugin/importer

test:
	${GO} test ./...

clean:
	rm -f kvmImporter kvm-*.ptar
