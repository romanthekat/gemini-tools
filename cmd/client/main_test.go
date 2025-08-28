package main

import (
	"bufio"
	"net/url"
	"strings"
	"testing"
)

// Test getFullGeminiLink ensures proper handling of raw links.
func TestGetFullGeminiLink(t *testing.T) {
	tests := []struct {
		raw    string
		want   string
		hasErr bool
	}{
		{"gemini://example.com", "gemini://example.com:1965", false},
		{"example.com/path", "gemini://example.com:1965/path", false},
		{"gemini://example.com:1234/abc", "gemini://example.com:1234/abc", false},
		{"gemini://[::1]/", "gemini://[::1]:1965/", false},
		{"http://example.com", "", true}, // http not supported
	}

	for _, tt := range tests {
		got, err := getFullGeminiLink(tt.raw)
		if (err != nil) != tt.hasErr {
			t.Fatalf("unexpected error status for %q: got %v want %v", tt.raw, err, tt.hasErr)
		}
		if err == nil && got.String() != tt.want {
			t.Errorf("expected %q, got %q", tt.want, got.String())
		}
	}
}

// Test processLink parses a gemtext link line and updates state.
func TestProcessLink(t *testing.T) {
	baseURL, _ := url.Parse("gemini://example.com:1965")
	line := "=> /doc/gemtext.gmi Gemtext Document"

	state := NewState()
	if err := processLink(state, baseURL, line); err != nil {
		t.Fatalf("processLink returned error: %v", err)
	}
	if len(state.Links) != 1 {
		t.Fatalf("expected one link in state, got %d", len(state.Links))
	}
	expectedAbs := "gemini://example.com:1965/doc/gemtext.gmi"
	if state.Links[0] != expectedAbs {
		t.Errorf("link mismatch: expected %q, got %q", expectedAbs, state.Links[0])
	}
}

// Test getResponse parses a Gemini response header and body.
func TestGetResponse(t *testing.T) {
	// Simulate a successful response with text/gemini mime type.
	header := "20 text/gemini\r\n"
	body := "Hello World\n=> gemini://example.com:1965/next Next Page\n"
	reader := bufio.NewReader(strings.NewReader(header + body))

	status, meta, data, err := getResponse(reader)
	if err != nil {
		t.Fatalf("getResponse error: %v", err)
	}
	if status != StatusSuccess {
		t.Errorf("expected status %d, got %d", StatusSuccess, status)
	}
	if meta != "text/gemini" {
		t.Errorf("expected meta %q, got %q", "text/gemini", meta)
	}
	expectedBody := []byte(body)
	if string(data) != string(expectedBody) {
		t.Errorf("body mismatch: expected %q, got %q", string(expectedBody), string(data))
	}

	// Simulate a redirect response.
	redirectHeader := "31 gemini://example.com:1965/redirect\r\n"
	rReader := bufio.NewReader(strings.NewReader(redirectHeader))
	status, meta, data, err = getResponse(rReader)
	if err != nil {
		t.Fatalf("getResponse error for redirect: %v", err)
	}
	if status != StatusRedirect {
		t.Errorf("expected redirect status %d, got %d", StatusRedirect, status)
	}
	if meta != "gemini://example.com:1965/redirect" {
		t.Errorf("expected meta %q, got %q", "gemini://example.com:1965/redirect", meta)
	}
	if len(data) != 0 {
		t.Errorf("expected empty body for redirect, got length %d", len(data))
	}
}

// Test getConn returns a TLS connection; here we only verify error handling
// with an invalid address (no network call is made in unit tests).
func TestGetConnInvalid(t *testing.T) {
	_, err := getConn("invalid:9999")
	if err == nil {
		t.Fatalf("expected error for invalid address, got nil")
	}
}

// Helper to ensure the State clearLinks works as expected.
func TestStateClearLinks(t *testing.T) {
	s := NewState()
	s.Links = []string{"a", "b"}
	s.clearLinks()
	if len(s.Links) != 0 {
		t.Errorf("expected Links slice cleared, got length %d", len(s.Links))
	}
}

// processUserInput tests for various input cases to guide future UI/protocol split.
func TestProcessUserInput(t *testing.T) {
	state := NewState()

	// Empty input -> do nothing
	if link, dn, err := processUserInput("", state); err != nil || !dn || link != nil {
		t.Fatalf("empty input unexpected: link=%v dn=%v err=%v", link, dn, err)
	}

	// Help -> do nothing
	if link, dn, err := processUserInput("h", state); err != nil || !dn || link != nil {
		t.Fatalf("help input unexpected: link=%v dn=%v err=%v", link, dn, err)
	}

	// 'g' shortcut
	link, dn, err := processUserInput("g", state)
	if err != nil || dn || link == nil {
		t.Fatalf("g input unexpected: link=%v dn=%v err=%v", link, dn, err)
	}
	if link.String() != "gemini://geminiprotocol.net:1965/" {
		t.Errorf("g shortcut link mismatch: %s", link.String())
	}

	// Out-of-range number
	state.Links = []string{"gemini://example.com:1965/a"}
	if link, dn, err := processUserInput("2", state); err != nil || !dn || link != nil {
		t.Fatalf("out-of-range unexpected: link=%v dn=%v err=%v", link, dn, err)
	}

	// Valid number selection
	link, dn, err = processUserInput("1", state)
	if err != nil || dn || link == nil {
		t.Fatalf("number selection unexpected: link=%v dn=%v err=%v", link, dn, err)
	}
	if link.String() != "gemini://example.com:1965/a" {
		t.Errorf("number link mismatch: %s", link.String())
	}

	// URL normalization without protocol
	link, dn, err = processUserInput("example.com/page", state)
	if err != nil || dn || link == nil {
		t.Fatalf("url normalization unexpected: link=%v dn=%v err=%v", link, dn, err)
	}
	if link.String() != "gemini://example.com:1965/page" {
		t.Errorf("normalized link mismatch: %s", link.String())
	}

	// Back navigation with insufficient history
	state.History = []string{"gemini://example.com:1965/a"}
	if link, dn, err := processUserInput("b", state); err != nil || !dn || link != nil {
		t.Fatalf("back insufficient unexpected: link=%v dn=%v err=%v", link, dn, err)
	}

	// Back navigation with history
	state.History = []string{"gemini://example.com:1965/first", "gemini://example.com:1965/second"}
	link, dn, err = processUserInput("b", state)
	if err != nil || dn || link == nil {
		t.Fatalf("back navigation unexpected: link=%v dn=%v err=%v", link, dn, err)
	}
	if link.String() != "gemini://example.com:1965/first" {
		t.Errorf("back link mismatch: %s", link.String())
	}
	if len(state.History) != 0 {
		t.Errorf("history not trimmed correctly, got %v", state.History)
	}
}

func TestProcessResponseStatuses(t *testing.T) {
	state := NewState()
	link, _ := url.Parse("gemini://example.com:1965/")

	// Unsupported statuses
	for _, st := range []int{StatusInput, StatusRedirect, StatusClientCertRequired} {
		resp := &Response{Status: st, Meta: "meta"}
		if err := processResponse(state, link, resp); err == nil {
			t.Errorf("expected error for status %d", st)
		}
	}

	// Failure statuses
	for _, st := range []int{StatusTemporaryFailure, StatusPermanentFailure} {
		resp := &Response{Status: st, Meta: "failure"}
		if err := processResponse(state, link, resp); err == nil || !strings.Contains(err.Error(), "ERROR:") {
			t.Errorf("expected ERROR: prefix for status %d, got %v", st, err)
		}
	}

	// Success flow
	resp := &Response{Status: StatusSuccess, Meta: GeminiMediaType, Body: []byte("# Title\n")}
	if err := processResponse(state, link, resp); err != nil {
		t.Fatalf("unexpected error for success: %v", err)
	}
}

func TestProcessSuccessfulResponseGemtext(t *testing.T) {
	state := NewState()
	state.Links = []string{"old"}
	link, _ := url.Parse("gemini://example.com:1965/base")

	body := strings.Join([]string{
		"# Header",
		"```",
		"=> /ignored ShouldNotBeParsed",
		"```",
		"=> /ok Parsed Link",
		"Normal text",
	}, "\n")

	resp := &Response{Status: StatusSuccess, Meta: GeminiMediaType, Body: []byte(body)}
	if err := processSuccessfulResponse(state, link, resp); err != nil {
		t.Fatalf("processSuccessfulResponse error: %v", err)
	}

	// Links cleared and only the parsed link added
	if len(state.Links) != 1 || !strings.Contains(state.Links[0], "/ok") {
		t.Errorf("links not processed as expected: %v", state.Links)
	}
	// History updated
	if got := state.History; len(got) != 1 || got[0] != link.String() {
		t.Errorf("history not updated: %v", got)
	}
}

func TestProcessSuccessfulResponsePlainText(t *testing.T) {
	state := NewState()
	state.Links = []string{"keep"}
	link, _ := url.Parse("gemini://example.com:1965/plain")

	resp := &Response{Status: StatusSuccess, Meta: "text/plain", Body: []byte("=> not a gemtext link\n")}
	if err := processSuccessfulResponse(state, link, resp); err != nil {
		t.Fatalf("processSuccessfulResponse error: %v", err)
	}
	// Links should be unchanged because not gemini media type
	if len(state.Links) != 1 || state.Links[0] != "keep" {
		t.Errorf("links should not be modified for plain text: %v", state.Links)
	}
	// History updated
	if got := state.History; len(got) != 1 || got[0] != link.String() {
		t.Errorf("history not updated for plain text: %v", got)
	}
}
