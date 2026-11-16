package main

import (
	"os"
	"testing"
)

// BenchmarkBuildIndex maps the checkout in $PRF_BENCH_DIR, e.g.
// PRF_BENCH_DIR=~/code/big go test -bench BuildIndex -run x
func BenchmarkBuildIndex(b *testing.B) {
	dir := os.Getenv("PRF_BENCH_DIR")
	if dir == "" {
		b.Skip("set PRF_BENCH_DIR to a checkout to map")
	}
	for b.Loop() {
		if err := buildIndex(dir, "bench", "main", b.TempDir()); err != nil {
			b.Fatal(err)
		}
	}
}
