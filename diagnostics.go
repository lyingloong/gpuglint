package main

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

func displayErrors(out io.Writer, s snapshot) error {
	// Remote stderr is untrusted terminal input. Keep the text of each
	// diagnostic on its own indented line and neutralize control characters.
	printable := func(s string) string {
		return strings.Map(func(r rune) rune {
			if !unicode.IsPrint(r) {
				return ' '
			}
			return r
		}, s)
	}
	var b strings.Builder
	for _, h := range s.Hosts {
		if h.Error == nil {
			continue
		}
		if b.Len() != 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s  [%s]\n  %s\n", printable(h.Alias), printable(h.Error.Code), printable(h.Error.Message))
	}
	if b.Len() == 0 {
		b.WriteString("No host errors reported.\n")
	}
	_, err := io.WriteString(out, b.String())
	return err
}
