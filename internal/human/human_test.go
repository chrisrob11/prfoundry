package human

import (
	"testing"
	"time"
)

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		1400 * time.Millisecond:                    "1s",
		4*time.Minute + 12*time.Second:             "4m12s",
		time.Hour + 3*time.Minute + 20*time.Second: "1h3m",
	} {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
}
