package cmd_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ratecap/cli/cmd"
)

func TestRootCmd_VersionFlagPrintsVersion(t *testing.T) {
	original := cmd.Version
	cmd.Version = "9.9.9-test"
	defer func() { cmd.Version = original }()

	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"--version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(out.String(), "9.9.9-test") {
		t.Errorf("expected --version output to contain overridden version %q, got %q", "9.9.9-test", out.String())
	}
}

func TestRootCmd_NoArgsPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected a no-args invocation to print usage/help, got %q", out.String())
	}
}

func TestRootCmd_UnknownSubcommandErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	root := cmd.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"not-a-real-subcommand"})

	if err := root.Execute(); err == nil {
		t.Fatal("expected an error for an unknown subcommand, got nil")
	}
}

// These exercise cmd.Run directly — the function main.go now delegates to —
// so the actual exit-code mapping is tested in-process, without needing
// os.Exit (which would kill the test binary itself).
func TestRun_ReturnsZeroExitCodeOnSuccess(t *testing.T) {
	original := cmd.Version
	cmd.Version = "9.9.9-test"
	defer func() { cmd.Version = original }()

	var out, errOut bytes.Buffer
	code := cmd.Run([]string{"--version"}, &out, &errOut)

	if code != 0 {
		t.Errorf("expected exit code 0 for --version, got %d (stderr: %q)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "9.9.9-test") {
		t.Errorf("expected stdout to contain the version, got %q", out.String())
	}
}

func TestRun_ReturnsNonZeroExitCodeOnError(t *testing.T) {
	var out, errOut bytes.Buffer
	code := cmd.Run([]string{"not-a-real-subcommand"}, &out, &errOut)

	if code != 1 {
		t.Errorf("expected exit code 1 for an unknown subcommand, got %d", code)
	}
	if errOut.Len() == 0 {
		t.Error("expected an error message written to stderr")
	}
}
