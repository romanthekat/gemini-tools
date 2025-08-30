# gemini-tools
personal tools for project gemini protocol

## cmd/client
Simple gemini client.  
`go run cmd/client/main.go`

Use `go test ./...` to validate current implementation.

## Crawler Plan
A detailed plan for a simple Gemini crawler is available in docs/crawler_plan.md. This plan covers storage layout, timestamps, queue management, throttling, error logging, link extraction, and a proposed Go package API for later implementation.

![client example](./docs/client_example.png)
