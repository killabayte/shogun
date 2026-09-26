package pipeline

import (
	"bytes"
	"embed"
	"text/template"

	"github.com/killabayte/shogun/internal/run"
)

//go:embed prompts/*.tmpl
var promptFiles embed.FS

var prompts = template.Must(template.ParseFS(promptFiles, "prompts/*.tmpl"))

type repoRef struct{ ID, Root, Head string }

type srcRef struct{ ID, Origin, Path, SHA string }

// promptData is everything a built-in prompt may show. Large material is passed by path.
type promptData struct {
	Stage, Role  string
	Revision     int
	Lang         string
	TaskPath     string
	Task         string
	Repos        []repoRef
	Inputs       []srcRef
	Web          []srcRef
	WebFailed    []srcRef // Path holds the fetch error
	Decisions    []Decision
	OpenFindings []run.Finding
	GateNotes    []string
	PrevPath     string   // planner: its previous revision of this stage
	DocPath      string   // reviewer: the revision under review
	ResearchPath string   // outline: the approved research
	Expected     []string // reviewer: requirement ids that need a coverage row
}

func renderPrompt(stage, role string, d promptData) (string, error) {
	var b bytes.Buffer
	if err := prompts.ExecuteTemplate(&b, stage+"."+role+".tmpl", d); err != nil {
		return "", err
	}
	return b.String(), nil
}
