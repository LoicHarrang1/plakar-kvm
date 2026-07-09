package main

import (
	"os"

	sdk "github.com/PlakarKorp/go-kloset-sdk"
	"github.com/LoicHarrang1/plakar-kvm/importer"
)

func main() {
	sdk.EntrypointImporter(os.Args, importer.NewImporter)
}
