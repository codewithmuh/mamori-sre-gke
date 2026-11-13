module github.com/codewithmuh/mamori-sre-gke/tools/loadtest

// Same Go as the other tools: the go line is the oldest Go this builds with, the toolchain line is
// what CI and local checks use.
go 1.27.0

toolchain go1.27.3

require (
	go.yaml.in/yaml/v3 v3.0.6
	golang.org/x/oauth2 v0.37.0
)

require (
	cloud.google.com/go/compute/metadata v0.10.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
)
