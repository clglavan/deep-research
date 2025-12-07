package search

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// SearXNGClient implements the Searcher interface for SearXNG
type SearXNGClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewSearXNGClient creates a new SearXNG client
func NewSearXNGClient(baseURL string) *SearXNGClient {
	return &SearXNGClient{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

type searxngResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// Search performs a search on SearXNG (page 1)
func (s *SearXNGClient) Search(query string) ([]Result, error) {
	return s.SearchWithPage(query, 1)
}

// SearchWithPage performs a paginated search on SearXNG
func (s *SearXNGClient) SearchWithPage(query string, page int) ([]Result, error) {
	params := url.Values{}
	params.Add("q", query)
	params.Add("format", "json")
	if page > 1 {
		params.Add("pageno", fmt.Sprintf("%d", page))
	}
	// params.Add("language", "en") // Remove language restriction to allow local results

	u := fmt.Sprintf("%s/search?%s", s.BaseURL, params.Encode())

	req, err := http.NewRequest("GET", u, nil) // SearXNG usually supports GET for JSON
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// User-Agent is often required
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	
	// Fix for 403 Forbidden: SearXNG bot detection requires X-Forwarded-For or X-Real-IP
	// when running behind a proxy or in certain Docker configurations.
	// Since we are calling it locally, we can set it to localhost.
	req.Header.Set("X-Real-IP", "127.0.0.1")
	req.Header.Set("X-Forwarded-For", "127.0.0.1")

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng returned status %d", resp.StatusCode)
	}

	// Debug: Print raw response if needed (commented out)
	// bodyBytes, _ := io.ReadAll(resp.Body)
	// fmt.Println(string(bodyBytes))
	// resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	var sResp searxngResponse
	if err := json.NewDecoder(resp.Body).Decode(&sResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	var results []Result
	for _, r := range sResp.Results {
		results = append(results, Result{
			Title:   r.Title,
			URL:     r.URL,
			Content: r.Content,
		})
	}

	return results, nil
}

// FetchPageContent fetches and extracts text content from a URL
func (s *SearXNGClient) FetchPageContent(pageURL string, maxLength int) (string, error) {
	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,ro;q=0.8")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch page: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("page returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read body: %w", err)
	}

	// Extract text from HTML (simple approach)
	text := extractTextFromHTML(string(body))
	
	// Truncate if too long
	if maxLength > 0 && len(text) > maxLength {
		text = text[:maxLength] + "..."
	}

	return text, nil
}

// extractTextFromHTML removes HTML tags and extracts readable text
func extractTextFromHTML(html string) string {
	// Remove script and style tags with their content
	scriptRe := regexp.MustCompile(`(?is)<script.*?</script>`)
	html = scriptRe.ReplaceAllString(html, "")
	
	styleRe := regexp.MustCompile(`(?is)<style.*?</style>`)
	html = styleRe.ReplaceAllString(html, "")
	
	// Remove HTML comments
	commentRe := regexp.MustCompile(`(?s)<!--.*?-->`)
	html = commentRe.ReplaceAllString(html, "")
	
	// Remove all HTML tags
	tagRe := regexp.MustCompile(`<[^>]*>`)
	text := tagRe.ReplaceAllString(html, " ")
	
	// Decode common HTML entities
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = strings.ReplaceAll(text, "&#39;", "'")
	
	// Collapse multiple whitespace into single space
	spaceRe := regexp.MustCompile(`\s+`)
	text = spaceRe.ReplaceAllString(text, " ")
	
	return strings.TrimSpace(text)
}

// ListingLink represents an individual item link extracted from an index page
type ListingLink struct {
	URL   string
	Title string
}

// ExtractListingLinks extracts individual item URLs from an index/category page
// Uses generic patterns to find links that look like individual item pages (not category pages)
func (s *SearXNGClient) ExtractListingLinks(pageURL string, maxLinks int) ([]ListingLink, error) {
	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch page: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("page returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	html := string(body)
	
	// Extract base URL for resolving relative links
	parsedURL, _ := url.Parse(pageURL)
	baseURL := fmt.Sprintf("%s://%s", parsedURL.Scheme, parsedURL.Host)
	
	// Generic patterns for individual item URLs (work across different sites/domains)
	// These patterns look for URLs that appear to be detail pages, not category/search pages
	itemPatterns := []string{
		// URLs ending with numeric ID (very common: /item/12345, /product-12345, /p/12345)
		`href=["']([^"']+/[a-zA-Z0-9_-]+-\d{4,}[^"']*)["']`,
		// URLs with /d/, /detail/, /item/, /view/, /show/ segments
		`href=["']([^"']*/(?:d|detail|item|view|show|product|article|post|ad|offer|oferta|anunt)/[^"']+)["']`,
		// URLs ending with alphanumeric ID (e.g., /X12345, /ABC123)
		`href=["']([^"']+/[A-Z][A-Z0-9]{5,}[^"']*)["']`,
		// URLs with slug + ID pattern (e.g., /some-title-here-12345)
		`href=["']([^"']+/[a-z0-9-]{10,}-\d{3,}[^"']*)["']`,
		// URLs ending with .html that have a slug (detail pages often end in .html)
		`href=["']([^"']+/[a-z0-9-]{5,}\.html)["']`,
	}
	
	seen := make(map[string]bool)
	var links []ListingLink
	
	for _, pattern := range itemPatterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindAllStringSubmatch(html, -1)
		
		for _, match := range matches {
			if len(match) < 2 {
				continue
			}
			href := match[1]
			
			// Skip if already seen
			if seen[href] {
				continue
			}
			
			// Resolve relative URLs
			fullURL := href
			if strings.HasPrefix(href, "/") {
				fullURL = baseURL + href
			} else if !strings.HasPrefix(href, "http") {
				continue // Skip non-http links
			}
			
			// Skip URLs that look like category/search/navigation pages
			if isLikelyCategoryPage(fullURL) {
				continue
			}
			
			// Must be same domain as the source page
			linkParsed, err := url.Parse(fullURL)
			if err != nil || linkParsed.Host != parsedURL.Host {
				continue
			}
			
			seen[fullURL] = true
			
			// Extract title from URL
			title := extractTitleFromURL(fullURL)
			
			links = append(links, ListingLink{URL: fullURL, Title: title})
			
			if len(links) >= maxLinks {
				return links, nil
			}
		}
	}
	
	return links, nil
}

// isLikelyCategoryPage checks if a URL looks like a category/search page rather than an item page
func isLikelyCategoryPage(urlStr string) bool {
	lowerURL := strings.ToLower(urlStr)
	
	// Category/navigation indicators
	categoryIndicators := []string{
		"/category/", "/categories/", "/tag/", "/tags/",
		"/search", "/results", "/browse", "/list",
		"/page/", "/p=", "page=", "pagina=",
		"/filter", "/sort", "/order",
		"/login", "/register", "/signup", "/account",
		"/contact", "/about", "/help", "/faq",
		"/terms", "/privacy", "/cookie",
	}
	
	for _, indicator := range categoryIndicators {
		if strings.Contains(lowerURL, indicator) {
			return true
		}
	}
	
	// URLs with many query parameters are often search/filter pages
	if strings.Count(urlStr, "&") > 2 {
		return true
	}
	
	return false
}

// extractTitleFromURL creates a readable title from a listing URL
func extractTitleFromURL(listingURL string) string {
	parsedURL, err := url.Parse(listingURL)
	if err != nil {
		return listingURL
	}
	
	// Get the last path segment and clean it up
	parts := strings.Split(strings.Trim(parsedURL.Path, "/"), "/")
	if len(parts) == 0 {
		return listingURL
	}
	
	lastPart := parts[len(parts)-1]
	// Remove file extensions
	lastPart = strings.TrimSuffix(lastPart, ".html")
	// Replace hyphens/underscores with spaces
	lastPart = strings.ReplaceAll(lastPart, "-", " ")
	lastPart = strings.ReplaceAll(lastPart, "_", " ")
	
	return lastPart
}

// LinkExtractor interface for extracting listing links
type LinkExtractor interface {
	ExtractListingLinks(pageURL string, maxLinks int) ([]ListingLink, error)
}
