module github.com/codewithmuh/mamori-sre-gke/tools/slo-gen

// Same Go lines as tools/spend-guard: the oldest Go this builds with, and the
// toolchain CI and local checks use.
go 1.27.0

toolchain go1.27.3

require go.yaml.in/yaml/v3 v3.0.6
