package main

import (
	"os"

	sdk "github.com/PlakarKorp/go-kloset-sdk"
	"github.com/LoicHarrang1/plakar-kvm/exporter"
)

func main() {
	sdk.EntrypointExporter(os.Args, exporter.NewExporter)
}
