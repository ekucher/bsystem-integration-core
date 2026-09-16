package ai

import (
	"strings"
	"testing"
)

// Redaction runs on every fragment and on every question, so its cost is paid
// on the request path rather than in a background job.
func BenchmarkRedactOrdinaryText(b *testing.B) {
	text := "Northwind Trading is a key account in the Kyiv region. The backup " +
		"failed at 02:14 with exit code 1 and the tokenizer produced 40 tokens."
	b.ReportAllocs()
	for b.Loop() {
		_ = Redact(text)
	}
}

func BenchmarkRedactWithCredentials(b *testing.B) {
	text := "password: hunter2\nAuthorization: Bearer " + fakeAPIKey() +
		"\nx-api-key=" + fakeToken()
	b.ReportAllocs()
	for b.Loop() {
		_ = Redact(text)
	}
}

func BenchmarkBuildPrompt(b *testing.B) {
	fragments := make([]Fragment, 0, MaxSources)
	for i := 0; i < MaxSources; i++ {
		fragments = append(fragments, Fragment{
			EntityType: "incident", GlobalID: "INC-000001",
			Title:          "Nightly backup has not run for two days",
			Detail:         "incident, severity high, status in_progress",
			Classification: ClassInternal,
		})
	}
	request := Request{Question: strings.Repeat("why ", 100), Fragments: fragments}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := BuildPrompt(request); err != nil {
			b.Fatal(err)
		}
	}
}
