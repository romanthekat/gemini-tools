package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	Port            = "1965"
	GeminiMediaType = "text/gemini"

	StatusInput    = 1
	StatusSuccess  = 2
	StatusRedirect = 3

	StatusTemporaryFailure = 4
	StatusPermanentFailure = 5

	StatusClientCertRequired = 6

	LinkPrefix   = "=>"
	Protocol     = "gemini://"
	MaxRedirects = 4
)

type State struct {
	Links   []string
	History []string
}

func (s *State) clearLinks() {
	s.Links = make([]string, 0, 100)
}

func main() {
	reader := bufio.NewReader(os.Stdin)

	state := &State{}

	fmt.Println("gemini://url\topen url")
	fmt.Println("number\t\topen link by number")
	fmt.Println("b\t\tgo back")
	fmt.Println("q\t\tquit")
	fmt.Println()

	for {
		input, err := getUserInput(reader)
		if err != nil {
			fmt.Println("user input read failed:", err)
			os.Exit(-1)
		}

		link, doNothing, err := processUserInput(input, state)
		if err != nil {
			fmt.Println("error processing user input:", err)
			continue
		}
		if doNothing {
			continue
		}

		status, meta, bodyBytes, err := doRequest(link)
		if err != nil {
			fmt.Println("request failed:", err)
			continue
		}

		err = processResponse(state, link, status, meta, bodyBytes)
		if err != nil {
			fmt.Println("error processing response:", err)
			continue
		}
	}
}

func processResponse(state *State, link *url.URL,
	status int, meta string, bodyBytes []byte) error {
	switch status {
	case StatusInput, StatusRedirect, StatusClientCertRequired:
		return fmt.Errorf("unsupported status: %s", meta)

	case StatusSuccess:
		if !strings.HasPrefix(meta, "text/") {
			return fmt.Errorf("unsupported type: %s", meta)
		}

		body := string(bodyBytes)
		if strings.HasPrefix(meta, GeminiMediaType) {
			state.clearLinks()
			preformatted := false

			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(line, "```") {
					preformatted = !preformatted
				} else if preformatted {
					fmt.Println(line)
				} else if strings.HasPrefix(line, LinkPrefix) {
					line = line[2:]
					parts := strings.Fields(line)
					parsedLink, err := url.Parse(parts[0])
					if err != nil {
						return fmt.Errorf("parsing absoluteLink failed: %w", err)
					}

					absoluteLink := link.ResolveReference(parsedLink).String()
					var linkNum string
					if len(parts) == 1 {
						linkNum = absoluteLink
					} else {
						linkNum = strings.Join(parts[1:], " ")
					}

					state.Links = append(state.Links, absoluteLink)
					fmt.Printf("\033[34m[%d] %s\033[0m\n", len(state.Links), linkNum)
				} else {
					fmt.Println(line)
				}
			}
		} else {
			// print as is
			fmt.Print(body)
		}

		state.History = append(state.History, link.String())

	case StatusTemporaryFailure, StatusPermanentFailure:
		return fmt.Errorf("ERROR: %s", meta)
	}

	return nil
}

func getUserInput(reader *bufio.Reader) (string, error) {
	fmt.Print("> ")
	input, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(input), err
}

func processUserInput(input string, state *State) (*url.URL, bool, error) {
	linkRaw := ""

	switch input {
	case "":
		return nil, true, nil

	case "q":
		os.Exit(0)

	case "b":
		if len(state.History) < 2 {
			fmt.Println("\033[31mNo history yet\033[0m")
			return nil, true, nil
		}

		linkRaw = state.History[len(state.History)-2]
		state.History = state.History[:len(state.History)-2]

	default:
		index, err := strconv.Atoi(input)
		if err != nil {
			// Treat this as a URL
			linkRaw = input
			if !strings.HasPrefix(linkRaw, Protocol) {
				linkRaw = Protocol + linkRaw
			}
		} else {
			linkRaw = state.Links[index-1]
		}
	}

	link, err := getFullGeminiLink(linkRaw)
	if err != nil {
		return nil, false, fmt.Errorf("error generating gemini URL: %w", err)
	}

	return link, false, nil
}

func getFullGeminiLink(linkRaw string) (*url.URL, error) {
	link, err := url.Parse(linkRaw)
	if err != nil {
		return link, fmt.Errorf("error parsing URL: %w", err)
	}

	if !strings.HasSuffix(link.Host, Port) {
		link.Host = link.Host + ":" + Port
	}

	return link, nil
}

func doRequest(link *url.URL) (status int, meta string, body []byte, err error) {
	redirectsLeft := MaxRedirects

	for {
		conn, err := getConn(link.Host)
		if err != nil {
			return status, meta, body, fmt.Errorf("connection failed: %w", err)
		}
		defer conn.Close()

		_, err = conn.Write([]byte(link.String() + "\r\n"))
		if err != nil {
			return status, meta, body, fmt.Errorf("sending request url failed: %w", err)
		}

		status, meta, body, err = getResponse(conn)
		if err != nil {
			return status, meta, body, err
		}

		if status == StatusRedirect {
			if redirectsLeft == 0 {
				return status, meta, body, fmt.Errorf("too many redirects, last url: %s", meta)
			}

			link, err = getFullGeminiLink(meta)
			if err != nil {
				return status, meta, body, fmt.Errorf("error generating gemini URL: %w", err)
			}

			redirectsLeft -= 1
			continue
		}

		return status, meta, body, err
	}
}

func getConn(addr string) (io.ReadWriteCloser, error) {
	dialer := &net.Dialer{Timeout: 4 * time.Second}

	conn, err := tls.DialWithDialer(
		dialer,
		"tcp", addr,
		&tls.Config{InsecureSkipVerify: true},
	)

	return conn, err
}

func getResponse(conn io.Reader) (status int, meta string, body []byte, err error) {
	reader := bufio.NewReader(conn)

	// 20 text/gemini
	// 20 text/gemini; charset=utf-8
	responseHeader, err := reader.ReadString('\n')
	if err != nil {
		return status, meta, body, fmt.Errorf("response header read failed: %w", err)
	}
	responseHeader = strings.TrimSpace(responseHeader)
	fmt.Println("responseHeader:", responseHeader)

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
