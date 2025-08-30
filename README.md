# gemini-tools
personal tools for project gemini protocol

## cmd/client
Simple gemini client.  
`go run cmd/client/main.go`

![client example](./docs/client_example.png)

## cmd/localclient
Simple local database reader for pages crawled by the crawler. UI mirrors cmd/client (colors, hotkeys q/h/g/b, numbered links).  
If a requested page is not present locally, it prints an error and appends the canonical URL to the queue file for crawler to process.

Run:
`go run cmd/localclient/main.go --db=data --queue=queue.txt`

Use `go test ./...` to validate current implementation.


## cmd/crawler
Simple crawler that will crawl a list of pages and save them to a local database.  
Can be also read offline using `cmd/localclient`.
