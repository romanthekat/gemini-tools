package gemini

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

type errReader struct{}

func (e errReader) Read(p []byte) (int, error) { return 0, errors.New("forced read error") }

func TestGetResponseNonNumericStatus(t *testing.T) {
	head := "x0 meta\r\n"
	reader := bufio.NewReader(strings.NewReader(head))
	_, _, _, err := GetResponse(reader)
	if err == nil || !strings.Contains(err.Error(), "response code parsing failed") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestGetResponseUnknownStatus(t *testing.T) {
	head := "90 something\r\n" // status '9' is unknown in our switch
	reader := bufio.NewReader(strings.NewReader(head))
	_, _, _, err := GetResponse(reader)
	if err == nil || !strings.Contains(err.Error(), "unknown response status") {
		t.Fatalf("expected unknown status error, got %v", err)
	}
}

func TestGetResponseBodyReadError(t *testing.T) {
	head := "20 text/gemini\r\n"
	reader := bufio.NewReader(io.MultiReader(strings.NewReader(head), errReader{}))
	_, _, _, err := GetResponse(reader)
	if err == nil || !strings.Contains(err.Error(), "response body reading failed") {
		t.Fatalf("expected body reading error, got %v", err)
	}
}

func TestGetFullGeminiLinkInvalidURL(t *testing.T) {
	_, err := GetFullGeminiLink("gemini://%zz")
	if err == nil || !strings.Contains(err.Error(), "error parsing URL") {
		t.Fatalf("expected URL parsing error, got %v", err)
	}
}
