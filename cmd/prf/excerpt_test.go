package main

import (
	"fmt"
	"strings"
	"testing"
)

// realVerbose is real go test -v output: TestA runs in parallel, so its log
// comes in pieces under === NAME, and TestTable has a passing and a failing
// subtest.
const realVerbose = `=== RUN   TestA
=== PAUSE TestA
=== RUN   TestB
=== PAUSE TestB
=== RUN   TestTable
=== RUN   TestTable/ok
    a_test.go:22: ok log
=== RUN   TestTable/bad
    a_test.go:23: bad log
    a_test.go:23: bad failed
--- FAIL: TestTable (0.00s)
    --- PASS: TestTable/ok (0.00s)
    --- FAIL: TestTable/bad (0.00s)
=== CONT  TestA
=== CONT  TestB
=== NAME  TestA
    a_test.go:10: A first detail
=== NAME  TestB
    a_test.go:17: B noise
--- PASS: TestB (0.00s)
=== NAME  TestA
    a_test.go:12: A second
--- FAIL: TestA (0.00s)
FAIL
FAIL	gv	0.256s
FAIL
`

func manyErrors(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "x.go:%d:1: e%d\n", i, i)
	}
	return b.String()
}

func TestExcerptKeepsTheFirstFailures(t *testing.T) {
	// A build error and more failed tests than fit: the excerpt is cut from
	// the end, so the build error and the first tests stay.
	var b strings.Builder
	b.WriteString("app/x.go:3:1: undefined: foo\n")
	for i := range 15 {
		fmt.Fprintf(&b, "--- FAIL: Test%02d (0.00s)\n", i)
		for k := range 40 {
			fmt.Fprintf(&b, "    n_test.go:%d: detail %d\n", k, k)
		}
	}
	got, _ := excerpt(b.String())
	if !strings.HasPrefix(got, "app/x.go:3:1: undefined: foo") || !strings.Contains(got, "--- FAIL: Test00") {
		t.Errorf("lost the first failures:\n%.300s", got)
	}
	if !strings.HasSuffix(got, "more lines") || strings.Count(got, "\n") > excerptLines {
		t.Errorf("not capped at %d lines with a note: %d lines, ends %q", excerptLines, strings.Count(got, "\n"), got[len(got)-30:])
	}
	if s := headOf("a\nb", 5, 100); s != "a\nb" {
		t.Errorf("headOf left out nothing, but = %q", s)
	}
	if s := headOf("aaaa\nbbbb\ncccc", 5, 10); s != "aaaa\nbbbb\n... 1 more lines" {
		t.Errorf("headOf by bytes = %q", s)
	}
}

func TestExcerpt(t *testing.T) {
	var many strings.Builder
	for i := range excerptBlocks + 3 {
		fmt.Fprintf(&many, "--- FAIL: Test%d (0.00s)\n    x_test.go:%d: bad\n", i, i+1)
	}
	long := "--- FAIL: TestLong (0.00s)\n" + strings.Repeat("    x_test.go:1: noise\n", excerptPerBlock+10)

	for _, tt := range []struct {
		name, out string
		known     bool
		want      []string // in the excerpt
		not       []string // left out
	}{
		{
			name: "build errors, each once",
			out: "# app/store\nstore/db.go:12:5: undefined: conn\nstore/db.go:12:5: undefined: conn\n" +
				"store/db.go:30:2: missing return\nFAIL\tapp/store [build failed]\n",
			known: true,
			want:  []string{"store/db.go:12:5: undefined: conn", "store/db.go:30:2: missing return", "FAIL\tapp/store [build failed]"},
		},
		{
			name:  "vet and linters",
			out:   "main.go:9:2: fmt.Printf format %d has arg s of wrong type string (printf)\n",
			known: true,
			want:  []string{"main.go:9:2: fmt.Printf format"},
		},
		{
			name: "a failed test, without -v",
			out: "ok  \tapp/a\t0.10s\n--- FAIL: TestAdd (0.00s)\n    add_test.go:7: Add(2, 3) = -1, want 5\n" +
				"--- FAIL: TestSub (0.00s)\n    sub_test.go:9: wrong\nFAIL\nFAIL\tapp/b\t0.21s\nok  \tapp/c\t0.30s\n",
			known: true,
			want:  []string{"--- FAIL: TestAdd", "add_test.go:7: Add(2, 3) = -1, want 5", "--- FAIL: TestSub", "sub_test.go:9: wrong", "FAIL\tapp/b"},
			not:   []string{"app/a", "app/c"},
		},
		{
			name: "a failed test, with -v: its log comes first",
			out: "=== RUN   TestOK\n--- PASS: TestOK (0.00s)\n=== RUN   TestAdd\n    add_test.go:5: trying 2+3\n" +
				"    add_test.go:7: Add(2, 3) = -1, want 5\n--- FAIL: TestAdd (0.00s)\n=== RUN   TestLater\n--- PASS: TestLater (0.00s)\nFAIL\nFAIL\tapp\t0.2s\n",
			known: true,
			want:  []string{"--- FAIL: TestAdd", "add_test.go:5: trying 2+3", "add_test.go:7: Add(2, 3) = -1, want 5"},
			not:   []string{"TestOK", "TestLater"},
		},
		{
			name: "subtests",
			out: "--- FAIL: TestTable (0.00s)\n    --- FAIL: TestTable/empty (0.00s)\n        table_test.go:20: got 1\n" +
				"    --- PASS: TestTable/one (0.00s)\nFAIL\nFAIL\tapp\t0.1s\n",
			known: true,
			want:  []string{"--- FAIL: TestTable/empty", "table_test.go:20: got 1"},
		},
		{
			name: "a panic: its first goroutine only",
			out: "panic: runtime error: index out of range [3] with length 3\n\ngoroutine 7 [running]:\napp.pick(...)\n" +
				"\t/src/app/pick.go:14\napp.TestPick(0x1)\n\t/src/app/pick_test.go:9 +0x1c\n\ngoroutine 1 [chan receive]:\n" +
				"testing.(*T).Run(0x2)\n\t/go/src/testing/testing.go:1750\nFAIL\tapp\t0.3s\n",
			known: true,
			want:  []string{"panic: runtime error: index out of range", "goroutine 7 [running]:", "/src/app/pick.go:14", "FAIL\tapp"},
			not:   []string{"goroutine 1 [chan receive]", "testing.go:1750"},
		},
		{
			name: "a data race",
			out: "==================\nWARNING: DATA RACE\nWrite at 0x00c by goroutine 8:\n  app.(*cache).put()\n" +
				"      /src/app/cache.go:22\nPrevious read at 0x00c by goroutine 7:\n  app.(*cache).get()\n      /src/app/cache.go:15\n" +
				"==================\n--- FAIL: TestCache (0.01s)\n    testing.go:1490: race detected during execution of test\nFAIL\n",
			known: true,
			want:  []string{"WARNING: DATA RACE", "cache.go:22", "Previous read", "cache.go:15", "--- FAIL: TestCache"},
		},
		{
			name:  "too many failures",
			out:   many.String(),
			known: true,
			want:  []string{"--- FAIL: Test0 ", fmt.Sprintf("--- FAIL: Test%d ", excerptBlocks-1), "... 3 more failures"},
			not:   []string{fmt.Sprintf("--- FAIL: Test%d ", excerptBlocks)},
		},
		{
			name:  "a long failure is cut",
			out:   long,
			known: true,
			want:  []string{"--- FAIL: TestLong", "... 11 more lines"},
		},
		{
			name:  "real go test -v, with t.Parallel and subtests",
			out:   realVerbose,
			known: true,
			want:  []string{"--- FAIL: TestA", "A first detail", "A second", "--- FAIL: TestTable/bad", "bad log", "bad failed", "FAIL\tgv"},
			not:   []string{"B noise", "ok log", "--- PASS"},
		},
		{
			name:  "more errors than fit",
			out:   manyErrors(excerptPerBlock + 10),
			known: true,
			want:  []string{"x.go:1:1: e1", fmt.Sprintf("x.go:%d:1: e%d", excerptPerBlock, excerptPerBlock), "... 10 more errors"},
			not:   []string{fmt.Sprintf("e%d\n", excerptPerBlock+1)},
		},
		{
			name:  "a panic with no stack doesn't swallow what follows",
			out:   "panic: printed by a test, not a crash\n" + "--- FAIL: TestZ (0.00s)\n" + strings.Repeat("    z_test.go:1: z\n", 70) + "FAIL\tapp\t0.1s\n",
			known: true,
			want:  []string{"panic: printed by a test", "--- FAIL: TestZ", "... 11 more lines", "FAIL\tapp"},
		},
		{
			name:  "a race report cut off by a timeout",
			out:   "WARNING: DATA RACE\nWrite at 0x1 by goroutine 3:\n  app.put()\n--- FAIL: TestRace (0.00s)\n    race_test.go:3: boom\n",
			known: true,
			want:  []string{"WARNING: DATA RACE", "app.put()", "--- FAIL: TestRace", "race_test.go:3: boom"},
		},
		{name: "not Go", out: "make: *** [test] Error 2\nsomething broke\n"},
		{name: "a passing run", out: "ok  \tapp\t0.1s\n"},
		{name: "nothing", out: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, known := excerpt(tt.out)
			if known != tt.known {
				t.Fatalf("known = %t, want %t; excerpt:\n%s", known, tt.known, got)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q:\n%s", w, got)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(got, n) {
					t.Errorf("kept %q:\n%s", n, got)
				}
			}
			if strings.Count(got, "store/db.go:12:5") > 1 {
				t.Errorf("repeated an error:\n%s", got)
			}
		})
	}
}
