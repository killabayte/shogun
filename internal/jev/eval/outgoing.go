package eval

import (
	"fmt"
	"io"

	"github.com/killabayte/shogun/internal/jev"
)

// Outgoing writes the audit text of every request the evaluation would make — exactly what the
// guard inside the client scans: the state's strings as plain text and every question with its
// options or levels — with the guard's verdict per item. It makes no request and never includes
// the key. A human reads this before the first live run; the returned count is the number of
// requests the guard would refuse (each such item is then skipped, and the run cannot pass).
func Outgoing(w io.Writer, cases []Case, deny []jev.Pattern) (refused int, err error) {
	for _, c := range cases {
		_, items, err := Load(c)
		if err != nil {
			return refused, fmt.Errorf("%s: %w", c.Name, err)
		}
		for _, it := range items {
			text := jev.Audit(it.State, it.Questions)
			ms := jev.Scan(text, deny)
			verdict := "guard: clean"
			if len(ms) > 0 {
				refused++
				verdict = "guard: REFUSED — " + jev.Describe(ms)
			}
			fmt.Fprintf(w, "=== %s / %s (%s) — %d chars, %s\n%s\n", c.Name, it.ID, it.Kind, len(text), verdict, text)
		}
	}
	return refused, nil
}
