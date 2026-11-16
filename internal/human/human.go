// Package human formats values for prf's progress lines and tables: a
// text's first line, a duration, and an amount in dollars.
package human

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// OneLine returns s's first non-empty line, shortened to max characters.
func OneLine(s string, max int) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > max {
				line = line[:max-3] + "..."
			}
			return line
		}
	}
	return ""
}

// Duration formats d as 45s, 4m12s, or 1h3m.
func Duration(d time.Duration) string {
	if d < time.Hour {
		return d.Round(time.Second).String()
	}
	return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
}

// USD formats dollars to the cent, or more precisely below a cent.
func USD(v float64) string {
	if v > 0 && v < 0.01 {
		return "$" + strconv.FormatFloat(v, 'f', -1, 64)
	}
	return fmt.Sprintf("$%.2f", v)
}
