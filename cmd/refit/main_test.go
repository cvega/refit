package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestHelpAndOfflineGuards(t *testing.T) {
	for _, command := range [][]string{{"help"}, {"prepare", "-help"}} {
		var output bytes.Buffer
		_ = run(context.Background(), command, &output, &output)
		if output.Len() == 0 {
			t.Fatal("missing help")
		}
		if command[0] == "help" && !strings.HasPrefix(output.String(), "refit COMMAND [flags]") {
			t.Fatal("help does not identify the refit command")
		}
	}
	t.Setenv("GH_PAT", "")
	t.Setenv("GH_SOURCE_PAT", "")
	for _, command := range [][]string{{"unknown"}, {"export"}, {"prepare"}, {"stage"}, {"lfs-push"}, {"verify"}, {"status"}} {
		var output bytes.Buffer
		if err := run(context.Background(), command, &output, &output); err == nil {
			t.Fatalf("accepted incomplete %v", command)
		}
	}
	var output bytes.Buffer
	_ = run(context.Background(), []string{"prepare", "-help"}, &output, &output)
	if !strings.Contains(output.String(), "1000000000") {
		t.Fatal("wrong default feature-flag threshold")
	}
}
