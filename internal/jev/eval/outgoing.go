package eval

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/killabayte/shogun/internal/jev"
)

// Outgoing writes every byte that the evaluation would send — each case, each item, the exact
// serialized state and the question texts — and runs the outbound guard over each state. It makes
// no request. A human reads this before the first live run; the returned count is the number of
// states the guard refused (each such item would be skipped, and the run would not pass).
func Outgoing(w io.Writer, cases []Case, deny []jev.Pattern) (refused int, err error) {
	for _, c := range cases {
		_, items, err := Load(c)
		if err != nil {
			return refused, fmt.Errorf("%s: %w", c.Name, err)
		}
		for _, it := range items {
			state, _ := json.MarshalIndent(it.State, "", " ")
			ms := jev.Scan(string(state), deny)
			verdict := "guard: clean"
			if len(ms) > 0 {
				refused++
				verdict = "guard: REFUSED — " + jev.Describe(ms)
			}
			fmt.Fprintf(w, "=== %s / %s (%s) — %d chars, %s\n", c.Name, it.ID, it.Kind, len(state), verdict)
			fmt.Fprintf(w, "--- state\n%s\n--- questions\n", state)
			for name, q := range it.Questions {
				fmt.Fprintf(w, "%s (%s): %s\n", name, q.Type, q.Instructions)
			}
			fmt.Fprintln(w)
		}
	}
	return refused, nil
}
