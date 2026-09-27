package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/guilhermesalviano/korchestrate/internal/artifact"
)

func TestListFailedOnly(t *testing.T) {
	base := t.TempDir()
	ok, err := artifact.New(base, "/repo", "good run")
	if err != nil {
		t.Fatal(err)
	}
	if err := ok.SetState(artifact.StateDone); err != nil {
		t.Fatal(err)
	}
	bad, err := artifact.New(base, "/repo", "bad run")
	if err != nil {
		t.Fatal(err)
	}
	if err := bad.AddError("executor", "codex", errors.New("codex exited 1: nope")); err != nil {
		t.Fatal(err)
	}
	if err := bad.Fail(errors.New("executor failed")); err != nil {
		t.Fatal(err)
	}

	dir := base
	cmd := newListCmd(&dir)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--failed"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(text, "good run") {
		t.Errorf("--failed included a done run:\n%s", text)
	}
	if !strings.Contains(text, "bad run") || !strings.Contains(text, "executor failed") {
		t.Errorf("--failed missing the failed run or its error:\n%s", text)
	}
}

func TestStatusShowsErrorHistory(t *testing.T) {
	base := t.TempDir()
	run, err := artifact.New(base, "/repo", "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := run.AddError("executor", "codex", errors.New("codex exited 1: nope")); err != nil {
		t.Fatal(err)
	}
	if err := run.Fail(errors.New("executor failed")); err != nil {
		t.Fatal(err)
	}

	dir := base
	cmd := newStatusCmd(&dir)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{run.ID})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"errors:    2", "executor", "codex exited 1", "kor logs " + run.ID + " --errors"} {
		if !strings.Contains(text, want) {
			t.Errorf("status missing %q:\n%s", want, text)
		}
	}
}
