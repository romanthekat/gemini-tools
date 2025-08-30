package gemini

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	Port            = "1965"
	GeminiMediaType = "text/gemini"

	StatusIncorrect = -1
	StatusInput     = 1
	StatusSuccess   = 2
	StatusRedirect  = 3

	StatusTemporaryFailure = 4
	StatusPermanentFailure = 5

	StatusClientCertRequired = 6

	Protocol     = "gemini://"
	MaxRedirects = 4
)

// Response represents a Gemini response
type Response struct {
	Status int
	Meta   string
	Body   []byte
}

func NewResponse(status int, meta string, body []byte) *Response {
	return &Response{status, meta, body}
}

func NewResponseEmpty() *Response {
	return NewResponse(StatusIncorrect, "", nil)
}

// GetFullGeminiLink normalizes raw link and ensures default port for Gemini
// TODO check if canonical url logic can be unified
func GetFullGeminiLink(linkRaw string) (*url.URL, error) {
	if strings.HasPrefix(linkRaw, "http") {
		return nil, fmt.Errorf("http(s) links aren't supported")
	}

	if !strings.HasPrefix(linkRaw, Protocol) {
		linkRaw = Protocol + linkRaw
	}

	link, err := url.Parse(linkRaw)
	if err != nil {
		return link, fmt.Errorf("error parsing URL: %w", err)
	}

	// Only add default port if no explicit port is present
	if link.Port() == "" {
		// Use JoinHostPort to be safe with IPv6 literals
		hostname := link.Hostname()
		if hostname == "" {
			// Fallback to raw host if parsing failed for some reason
			link.Host = link.Host + ":" + Port
		} else {
			link.Host = net.JoinHostPort(hostname, Port)
		}
	}

	return link, nil
}

// DoRequest performs a Gemini request with redirect handling
func DoRequest(link *url.URL) (*Response, error) {
	redirectsLeft := MaxRedirects

	for {
		conn, err := GetConn(link.Host)
		if err != nil {
			return NewResponseEmpty(), fmt.Errorf("connection failed: %w", err)
		}
		defer conn.Close()

		_, err = conn.Write([]byte(link.String() + "\r\n"))
		if err != nil {
			return NewResponseEmpty(), fmt.Errorf("sending request url failed: %w", err)
		}

		status, meta, body, err := GetResponse(conn)
		if err != nil {
			return NewResponse(status, meta, body), err
		}

		if status == StatusRedirect {
			if redirectsLeft == 0 {
				return NewResponse(status, meta, body), fmt.Errorf("too many redirects, last url: %s", meta)
			}

			link, err = GetFullGeminiLink(meta)
			if err != nil {
				return NewResponse(status, meta, body), fmt.Errorf("error generating gemini URL: %w", err)
			}

			redirectsLeft -= 1
			continue
		}

		return NewResponse(status, meta, body), err
	}
}

// GetResponse reads and parses a Gemini response from a connection
func GetResponse(conn io.Reader) (status int, meta string, body []byte, err error) {
	reader := bufio.NewReader(conn)

	// 20 text/gemini
	// 20 text/gemini; charset=utf-8
	responseHeader, err := reader.ReadString('\n')
	if err != nil {
		return status, meta, body, fmt.Errorf("response header read failed: %w", err)
	}
	responseHeader = strings.TrimSpace(responseHeader)
	// fmt.Println("responseHeader:", responseHeader) // suppress noisy output in library

	statusDelim := strings.Index(responseHeader, " ")

	status, err = strconv.Atoi(responseHeader[0:1])
	if err != nil {
		return status, meta, body, fmt.Errorf("response code parsing failed: %w", err)
	}

	meta = responseHeader[statusDelim+1:]

	switch status {
	case StatusInput, StatusRedirect,
		StatusTemporaryFailure, StatusPermanentFailure, StatusClientCertRequired:
		return status, meta, body, nil

	case StatusSuccess:
		body, err := io.ReadAll(reader)
		if err != nil {
			return status, meta, body, fmt.Errorf("response body reading failed: %w", err)
		}

		return status, meta, body, nil

	default:
		return status, meta, body, fmt.Errorf("unknown response status: %s", responseHeader)
	}
}

// GetConn dials a TLS connection to the given address
func GetConn(addr string) (io.ReadWriteCloser, error) {
	dialer := &net.Dialer{Timeout: 4 * time.Second}

	conn, err := tls.DialWithDialer(
		dialer,
		"tcp", addr,
		&tls.Config{InsecureSkipVerify: true},
	)

	return conn, err
}
