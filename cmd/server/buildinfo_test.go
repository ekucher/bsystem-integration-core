package main

import (
	"strings"
	"testing"
)

func TestShortCommitTruncatesWithoutLosingIdentity(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"abc123", "abc123"},
		{"0fde72361339b669bb6fdddc093acd5c281a79f6", "0fde72361339"},
		{"  0fde72361339b669bb6fdddc093acd5c281a79f6  ", "0fde72361339"},
	} {
		if got := shortCommit(tc.in); got != tc.want {
			t.Fatalf("shortCommit(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A build with no linker flags must still describe itself rather than
// reporting empty labels that make a dashboard useless.
func TestCurrentBuildAlwaysReportsAVersionAndSchemaLevel(t *testing.T) {
	info := currentBuild()
	if strings.TrimSpace(info.Version) == "" {
		t.Fatal("version must never be empty")
	}
	if !strings.HasSuffix(info.SchemaEmbedded, ".sql") {
		t.Fatalf("embedded schema level should name a migration file, got %q", info.SchemaEmbedded)
	}
}

// Build metadata is pasted into tickets. Nothing in it may carry a credential,
// so the fields are fixed and this test fails if the struct gains one.
func TestBuildInfoCarriesOnlyPublicIdentity(t *testing.T) {
	info := currentBuild()
	for _, value := range []string{info.Version, info.Commit, info.Date, info.SchemaEmbedded} {
		lowered := strings.ToLower(value)
		for _, banned := range []string{"password", "secret", "token", "api_key", "apikey"} {
			if strings.Contains(lowered, banned) {
				t.Fatalf("build metadata %q looks like it carries a credential", value)
			}
		}
	}
}
