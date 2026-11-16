package codeindex

import (
	"os"
	"testing"
)

// BenchmarkBuild maps the checkout in $PRF_BENCH_DIR, e.g.
// PRF_BENCH_DIR=~/code/big go test -bench Build -run x
func BenchmarkBuild(b *testing.B) {
	dir := os.Getenv("PRF_BENCH_DIR")
	if dir == "" {
		b.Skip("set PRF_BENCH_DIR to a checkout to map")
	}
	for b.Loop() {
		if err := Build(dir, "bench", "main", b.TempDir()); err != nil {
			b.Fatal(err)
		}
	}
}
