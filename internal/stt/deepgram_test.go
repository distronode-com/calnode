package stt

import (
	"strings"
	"testing"
)

func TestParseDeepgram(t *testing.T) {
	sample := `{"results":{"channels":[{"alternatives":[{"transcript":"Hello there. How are you?"}]}],` +
		`"utterances":[{"start":0.5,"end":1.2,"transcript":"Hello there.","speaker":0},` +
		`{"start":1.5,"end":2.4,"transcript":"How are you?","speaker":1}]}}`
	res, err := parseDeepgram(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Text != "Hello there. How are you?" {
		t.Errorf("text = %q", res.Text)
	}
	if len(res.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(res.Segments))
	}
	if res.Segments[1].Speaker != 1 || res.Segments[1].Text != "How are you?" {
		t.Errorf("seg[1] = %+v", res.Segments[1])
	}
}

func TestParseDeepgram_fallbackText(t *testing.T) {
	// No channel transcript present → Text falls back to the joined utterances.
	sample := `{"results":{"utterances":[{"start":0,"end":1,"transcript":"one","speaker":0},` +
		`{"start":1,"end":2,"transcript":"two","speaker":0}]}}`
	res, err := parseDeepgram(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Text != "one two" {
		t.Errorf("fallback text = %q", res.Text)
	}
}

// The base URL is configurable and the path/query are not: an operator picks a region,
// not a model or a set of transcription options.
func TestNewDeepgram_listenURLComposition(t *testing.T) {
	cases := map[string]string{
		"":                               DefaultBaseURL,
		"https://api.eu.deepgram.com":    "https://api.eu.deepgram.com",
		"https://api.eu.deepgram.com/":   "https://api.eu.deepgram.com",
		"  https://stt.internal:8443/  ": "https://stt.internal:8443",
		"http://localhost:9000":          "http://localhost:9000",
	}
	for in, wantBase := range cases {
		d := NewDeepgram("key", in)
		if d.BaseURL() != wantBase {
			t.Errorf("NewDeepgram(%q).BaseURL() = %q; want %q", in, d.BaseURL(), wantBase)
		}
		want := wantBase + listenPath
		if got := d.listenURL(); got != want {
			t.Errorf("NewDeepgram(%q).listenURL() = %q; want %q", in, got, want)
		}
	}
}

// The default must stay exactly what it was before the setting existed, and the query has
// to survive base-URL substitution — a dropped `diarize` would silently return a
// transcript with no speaker labels rather than an error.
func TestNewDeepgram_defaultIsUnchanged(t *testing.T) {
	const want = "https://api.deepgram.com/v1/listen?model=nova-2&smart_format=true&punctuate=true&diarize=true&utterances=true"
	if got := NewDeepgram("key", "").listenURL(); got != want {
		t.Errorf("default listenURL = %q; want %q", got, want)
	}
}
