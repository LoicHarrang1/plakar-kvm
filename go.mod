module github.com/PlakarKorp/integration-kvm

go 1.24.0

require (
	github.com/PlakarKorp/go-kloset-sdk v1.1.0-beta.1
	github.com/PlakarKorp/kloset v1.1.0-beta.2
	golang.org/x/sync v0.19.0
)

// Indirect dependencies are resolved by `go mod tidy` on a Linux host
// (the plugin targets Linux/KVM nodes). See README for the build steps.
