package main

import (
	"flag"
	"io"
	"reflect"
	"testing"
)

func TestP1Round2EndOfOptions(t *testing.T) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reviewer := fs.String("reviewer", "original", "reviewer")
	args := []string{"--", "task", "--reviewer", "literal-task-text"}
	got, err := parseArgs(fs, args)
	if err != nil {
		t.Fatal(err)
	}
	want := args[1:]
	if *reviewer != "original" || !reflect.DeepEqual(got, want) {
		t.Fatalf("-- must end option parsing; got args=%q reviewer=%q", got, *reviewer)
	}
}
