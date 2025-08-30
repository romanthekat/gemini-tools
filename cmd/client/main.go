package main

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/romanthekat/gemini-tools/internal/gemini"
)

const (
	LinkPrefix    = "=>"
	Header1Prefix = "#"
	Header2Prefix = "##"
	Header3Prefix = "###"
)

type State struct {
	Links   []string
	History []string
}

func (s *State) clearLinks() {
	s.Links = make([]string, 0, 100)
}

func NewState() *State {
	return &State{make([]string, 0, 100), make([]string, 0, 100)}
}

func main() {
	reader := bufio.NewReader(os.Stdin)

	state := NewState()

	printHelp()

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

		response, err := gemini.DoRequest(link)
		if err != nil {
			fmt.Println("request failed:", err)
			continue
		}

		err = processResponse(state, link, response)
		if err != nil {
			fmt.Println("error processing response:", err)
			continue
		}
	}
}

func printHelp() {
	fmt.Println("gemini://url\topen url")
	fmt.Println("number\t\topen link from current page by number")
	fmt.Println("b\t\tgo back")
	fmt.Println("q\t\tquit")
	fmt.Println("h\t\tprint this summary")
	fmt.Println("g\t\topen Project Gemini homepage")
	fmt.Println("l\t\tlinks from current page and history")
	fmt.Println()
}

func getUserInput(reader *bufio.Reader) (string, error) {
	fmt.Print("🔴➡ ")
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

	case "h":
		printHelp()
		return nil, true, nil

	case "g":
		linkRaw = "gemini://geminiprotocol.net:1965/"

	case "b":
		if len(state.History) < 2 {
			fmt.Println("\033[31mNo history yet\033[0m") //red
			return nil, true, nil
		}

		linkRaw = state.History[len(state.History)-2]
		state.History = state.History[:len(state.History)-2]

	case "l":
		fmt.Println("Links:")
		for i, l := range state.Links {
			fmt.Printf("[%d] \u001B[34m%s\033[0m\n", i+1, l)
			//fmt.Printf("[%d] \u001B[34m%s\033[0m\n", len(state.Links), linkNum) //blue
		}

		fmt.Println("\nHistory:")
		for _, l := range state.History {
			fmt.Printf("%s\n", l)
		}

		return nil, true, nil

	default:
		// Treat it as link number
		index, err := strconv.Atoi(input)
		if err != nil {
			// Treat this as a URL
			linkRaw = input
			if !strings.HasPrefix(linkRaw, gemini.Protocol) {
				linkRaw = gemini.Protocol + linkRaw
			}
		} else {
			if index > len(state.Links) {
				fmt.Println("\033[31mNo link with his number\033[0m") //red
				return nil, true, nil
			}

			linkRaw = state.Links[index-1]
		}
	}

	fmt.Println(">", linkRaw)
	link, err := gemini.GetFullGeminiLink(linkRaw)
	if err != nil {
		return nil, false, fmt.Errorf("error generating gemini URL: %w", err)
	}

	return link, false, nil
}

func processResponse(state *State, link *url.URL, response *gemini.Response) error {
	switch response.Status {
	case gemini.StatusInput, gemini.StatusRedirect, gemini.StatusClientCertRequired:
		return fmt.Errorf("unsupported status: %s", response.Meta)

	case gemini.StatusSuccess:
		err := processSuccessfulResponse(state, link, response)
		if err != nil {
			return err
		}

	case gemini.StatusTemporaryFailure, gemini.StatusPermanentFailure:
		return fmt.Errorf("ERROR: %s", response.Meta)
	}

	return nil
}

func processSuccessfulResponse(state *State, link *url.URL, response *gemini.Response) error {
	if !strings.HasPrefix(response.Meta, "text/") {
		return fmt.Errorf("unsupported type: %s", response.Meta)
	}

	body := string(response.Body)
	if strings.HasPrefix(response.Meta, gemini.GeminiMediaType) {
		state.clearLinks()
		preformatted := false

		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "```") {
				fmt.Println(line)
				preformatted = !preformatted
			} else if preformatted {
				fmt.Println(line)
			} else if strings.HasPrefix(line, LinkPrefix) {
				err := processLink(state, link, line)
				if err != nil {
					return err
				}
			} else if strings.HasPrefix(line, Header3Prefix) {
				fmt.Printf("\033[33m%s\033[0m\n", line) //orange
			} else if strings.HasPrefix(line, Header2Prefix) {
				fmt.Printf("\033[32m%s\033[0m\n", line) //green
			} else if strings.HasPrefix(line, Header1Prefix) {
				fmt.Printf("\033[31m%s\033[0m\n", line) //red
			} else {
				fmt.Println(line)
			}
		}
	} else {
		// print as is
		fmt.Print(body)
	}

	state.History = append(state.History, link.String())
	return nil
}

func processLink(state *State, link *url.URL, line string) error {
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
	fmt.Printf("[%d] \u001B[34m%s\033[0m\n", len(state.Links), linkNum) //blue

	return nil
}
