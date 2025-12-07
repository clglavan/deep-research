package search

// Result represents a single search result
type Result struct {
	Title       string
	URL         string
	Content     string
	FullContent string // Fetched page content (if available)
}

// Searcher is the interface for search engines
type Searcher interface {
	Search(query string) ([]Result, error)
	SearchWithPage(query string, page int) ([]Result, error) // Paginated search
}

// ContentFetcher is an interface for fetching page content
type ContentFetcher interface {
	FetchPageContent(url string, maxLength int) (string, error)
}
