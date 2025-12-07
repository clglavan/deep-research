package agent

import (
	"deep-research/pkg/llm"
	"deep-research/pkg/search"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// stripThinkTags removes <think>...</think> blocks from model output
func stripThinkTags(s string) string {
	if start := strings.Index(s, "<think>"); start != -1 {
		if end := strings.Index(s, "</think>"); end != -1 {
			s = s[end+8:]
		}
	}
	return strings.TrimSpace(s)
}

// Config holds the agent configuration
type Config struct {
	MaxLoops      int
	ParallelQuery int
	DeepMode      bool // When true, fetch and summarize each page individually
	ResultLinks   bool // When true, emphasize including direct links in results
	SimpleMode    bool // When true, use simple/quick research (not recommended)
	MinResults    int  // Minimum unique URLs to find before stopping
	DelayMs       int  // Milliseconds delay between HTTP requests (rate limiting)
	MaxPages      int  // Number of SearXNG result pages to fetch per query (0 = auto)
	ContextLength int  // LLM context length in tokens (for compression management)
}

// maxContextChars returns the estimated max characters based on context length
// Uses conservative 3.5 chars per token estimate
func (c Config) maxContextChars() int {
	if c.ContextLength <= 0 {
		return 32768 * 3 // Default 32k tokens * 3 chars = 98k chars
	}
	return int(float64(c.ContextLength) * 3.5)
}

// Source represents a single source URL with its title
type Source struct {
	Title string
	URL   string
}

// ResearchPlan contains the clarified query and research plan
type ResearchPlan struct {
	ClarifyingQuestions  []string `json:"clarifying_questions"`
	UnderstandingSummary string   `json:"understanding_summary"`
	ResearchSteps        []string `json:"research_steps"`
	ExpectedOutcome      string   `json:"expected_outcome"`
	SearchQueries        []string `json:"search_queries,omitempty"` // Pre-generated queries for exhaustive mode
}

// ResearchResult contains the final report and all sources
type ResearchResult struct {
	Report  string
	Sources []Source
}

// DeepResearcher is the main agent struct
type DeepResearcher struct {
	llmClient *llm.Client
	searcher  search.Searcher
	config    Config
	sources   []Source          // Track all sources found during research
	seenURLs  map[string]bool   // Deduplication: track URLs already processed
	mu        sync.Mutex        // Mutex for thread-safe access to seenURLs and sources
}

// NewDeepResearcher creates a new agent
func NewDeepResearcher(l *llm.Client, s search.Searcher, cfg Config) *DeepResearcher {
	return &DeepResearcher{
		llmClient: l,
		searcher:  s,
		config:    cfg,
		sources:   make([]Source, 0),
		seenURLs:  make(map[string]bool),
	}
}

// compressContext uses LLM to compress research context when it gets too large
// targetRatio is the target compression ratio (e.g., 0.5 for 50% reduction)
func (a *DeepResearcher) compressContext(context string, targetRatio float64) (string, error) {
	maxChars := a.config.maxContextChars()
	// Reserve space for the compression prompt itself (~500 chars) and response
	maxInputChars := int(float64(maxChars) * 0.6)
	
	// If context fits in a single compression call, do it directly
	if len(context) <= maxInputChars {
		return a.compressContextDirect(context, targetRatio)
	}
	
	// Context too large - use chunked compression
	fmt.Printf("📦 Context too large for single compression (%d chars), using chunked approach...\n", len(context))
	return a.compressContextChunked(context, targetRatio)
}

// compressContextDirect compresses context that fits within model limits
func (a *DeepResearcher) compressContextDirect(context string, targetRatio float64) (string, error) {
	targetChars := int(float64(len(context)) * targetRatio)
	
	prompt := fmt.Sprintf(`Compress this research context to ~%d characters. PRESERVE: URLs, prices, names, numbers, dates, specific facts. REMOVE: redundancy, verbose descriptions. Output ONLY compressed text:

%s`, targetChars, context)

	resp, err := a.llmClient.Chat([]llm.Message{
		{Role: "system", Content: "Compress text. Output only the result."},
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return context, fmt.Errorf("compression failed: %w", err)
	}

	compressed := stripThinkTags(resp)
	compressed = strings.TrimSpace(compressed)
	
	if len(compressed) < 200 {
		return context, fmt.Errorf("compression produced too small output (%d chars)", len(compressed))
	}
	
	fmt.Printf("📦 Compressed: %d → %d chars (%.0f%% reduction)\n", 
		len(context), len(compressed), (1-float64(len(compressed))/float64(len(context)))*100)
	
	return compressed, nil
}

// compressContextChunked splits large context into chunks, compresses each, then combines
func (a *DeepResearcher) compressContextChunked(context string, targetRatio float64) (string, error) {
	maxChars := a.config.maxContextChars()
	// Each chunk should be small enough to compress with room for prompt
	chunkSize := int(float64(maxChars) * 0.5)
	if chunkSize < 2000 {
		chunkSize = 2000
	}
	
	// Split context into chunks (try to split on double newlines to preserve structure)
	chunks := splitContextIntoChunks(context, chunkSize)
	fmt.Printf("📦 Split into %d chunks for compression\n", len(chunks))
	
	var compressedParts []string
	for i, chunk := range chunks {
		fmt.Printf("   Compressing chunk %d/%d (%d chars)...\n", i+1, len(chunks), len(chunk))
		
		compressed, err := a.compressContextDirect(chunk, targetRatio)
		if err != nil {
			// On error, aggressively truncate this chunk
			fmt.Printf("   ⚠️ Chunk %d compression failed, truncating\n", i+1)
			truncated := chunk
			if len(chunk) > chunkSize/4 {
				truncated = chunk[:chunkSize/4] + "\n[...truncated...]\n"
			}
			compressedParts = append(compressedParts, truncated)
			continue
		}
		compressedParts = append(compressedParts, compressed)
	}
	
	result := strings.Join(compressedParts, "\n\n---\n\n")
	
	// If still too large, recursively compress again
	maxTarget := int(float64(maxChars) * 0.6)
	if len(result) > maxTarget {
		fmt.Printf("📦 Combined result still too large (%d chars), compressing again...\n", len(result))
		return a.compressContext(result, targetRatio)
	}
	
	fmt.Printf("📦 Chunked compression complete: %d → %d chars (%.0f%% reduction)\n",
		len(context), len(result), (1-float64(len(result))/float64(len(context)))*100)
	
	return result, nil
}

// splitContextIntoChunks splits text into chunks, trying to break on paragraph boundaries
func splitContextIntoChunks(text string, maxChunkSize int) []string {
	if len(text) <= maxChunkSize {
		return []string{text}
	}
	
	var chunks []string
	remaining := text
	
	for len(remaining) > 0 {
		if len(remaining) <= maxChunkSize {
			chunks = append(chunks, remaining)
			break
		}
		
		// Try to find a good break point (double newline, then single newline, then space)
		chunk := remaining[:maxChunkSize]
		breakPoint := maxChunkSize
		
		// Look for double newline in last 20% of chunk
		searchStart := int(float64(maxChunkSize) * 0.8)
		if idx := strings.LastIndex(chunk[searchStart:], "\n\n"); idx != -1 {
			breakPoint = searchStart + idx + 2
		} else if idx := strings.LastIndex(chunk[searchStart:], "\n"); idx != -1 {
			breakPoint = searchStart + idx + 1
		} else if idx := strings.LastIndex(chunk[searchStart:], " "); idx != -1 {
			breakPoint = searchStart + idx + 1
		}
		
		chunks = append(chunks, remaining[:breakPoint])
		remaining = remaining[breakPoint:]
	}
	
	return chunks
}

// CreatePlan generates a research plan with clarifying questions
func (a *DeepResearcher) CreatePlan(topic string, additionalContext string) (ResearchPlan, error) {
	contextInfo := ""
	if additionalContext != "" {
		contextInfo = fmt.Sprintf("\n\nAdditional context from user:\n%s", additionalContext)
	}

	linkEmphasis := ""
	if a.config.ResultLinks {
		linkEmphasis = "\n\nIMPORTANT: The user wants results with DIRECT LINKS. Focus on finding specific listing/item URLs, not general category pages. Each result must have its own clickable link."
	}

	prompt := fmt.Sprintf(`You are a Deep Research AI planning a comprehensive research task.%s

User's research request: "%s"%s

Analyze this request and create a research plan. 

IMPORTANT: The user wants SPECIFIC, CONCRETE information - not general overviews. For example:
- If asking about products: exact prices, specific models, store links
- If asking about real estate: exact addresses, prices, property details, direct listing URLs
- If asking about comparisons: specific data points, benchmarks, specifications

Output a JSON object with:
1. "clarifying_questions": List of 2-4 questions to better understand what the user wants (ask about price ranges, locations, specific criteria, etc.)
2. "understanding_summary": A 1-2 sentence summary of what you understand the user wants, including any specific criteria
3. "research_steps": A list of 3-6 specific research steps - focus on finding EXACT listings/data, not general information pages
4. "expected_outcome": What SPECIFIC data the final report will contain (e.g., "A list of 10 properties with addresses, prices, and direct links")

Respond ONLY with valid JSON:
{
  "clarifying_questions": ["question1", "question2"],
  "understanding_summary": "...",
  "research_steps": ["step1", "step2", "step3"],
  "expected_outcome": "..."
}`, linkEmphasis, topic, contextInfo)

	resp, err := a.llmClient.Chat([]llm.Message{
		{Role: "system", Content: "You are a research planning assistant. Output only valid JSON."},
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return ResearchPlan{}, err
	}

	resp = stripThinkTags(resp)
	resp = strings.TrimPrefix(resp, "```json")
	resp = strings.TrimPrefix(resp, "```")
	resp = strings.TrimSuffix(resp, "```")
	resp = strings.TrimSpace(resp)

	var plan ResearchPlan
	if err := json.Unmarshal([]byte(resp), &plan); err != nil {
		return ResearchPlan{}, fmt.Errorf("failed to parse research plan: %w. Response: %s", err, resp)
	}

	return plan, nil
}

// Run executes the deep research loop (after plan is approved)
func (a *DeepResearcher) Run(topic string, plan ResearchPlan) (ResearchResult, error) {
	// Build context with the approved plan
	context := fmt.Sprintf(`User Query: %s

Research Plan:
- Understanding: %s
- Expected Outcome: %s
- Steps: %s

Knowledge so far:
None.`, topic, plan.UnderstandingSummary, plan.ExpectedOutcome, strings.Join(plan.ResearchSteps, "; "))
	
	a.sources = make([]Source, 0) // Reset sources for each run
	
	fmt.Printf("🧠 Starting Deep Research for: %s\n", topic)

	for i := 0; i < a.config.MaxLoops; i++ {
		fmt.Printf("\n--- Round %d/%d ---\n", i+1, a.config.MaxLoops)

		// Step 1: DECIDE
		decision, err := a.decide(context)
		if err != nil {
			return ResearchResult{}, fmt.Errorf("decision failed: %w", err)
		}

		if decision.FinalAnswer {
			fmt.Println("✅ Sufficient information gathered.")
			break
		}

		if len(decision.Queries) == 0 {
			fmt.Println("⚠️ No queries generated, but not final. Stopping to avoid loop.")
			break
		}

		// Step 2: ACT (Parallel Search)
		fmt.Printf("🔎 Searching for: %v\n", decision.Queries)
		searchResults := a.parallelSearch(decision.Queries)

		// Step 3: LEARN (Summarize)
		summary, err := a.summarize(topic, searchResults)
		if err != nil {
			return ResearchResult{}, fmt.Errorf("summarization failed: %w", err)
		}

		context += fmt.Sprintf("\n\nRound %d Findings:\n%s", i+1, summary)
	}

	// Final Report
	fmt.Println("\n✍️ Writing Final Report...")
	report, err := a.writeReport(topic, context)
	if err != nil {
		return ResearchResult{}, err
	}
	return ResearchResult{Report: report, Sources: a.sources}, nil
}

type decisionResponse struct {
	FinalAnswer bool     `json:"final_answer"`
	Queries     []string `json:"queries"`
}

func (a *DeepResearcher) decide(context string) (decisionResponse, error) {
	prompt := fmt.Sprintf(`You are a Deep Research AI. Your goal is to answer the user's query comprehensively.

Current Knowledge:
%s

Do you have enough information to answer the user request fully and in-depth?
If YES, set "final_answer" to true and "queries" to empty.
If NO, generate up to 3 search queries to find missing information.

Respond ONLY with a valid JSON object in this format:
{
  "final_answer": false,
  "queries": ["query 1", "query 2"]
}
`, context)

	resp, err := a.llmClient.Chat([]llm.Message{
		{Role: "system", Content: "You are a helpful research assistant. Output only JSON."},
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return decisionResponse{}, err
	}

	// Clean up response if it contains markdown code blocks
	resp = strings.TrimPrefix(resp, "```json")
	resp = strings.TrimPrefix(resp, "```")
	resp = strings.TrimSuffix(resp, "```")
	resp = strings.TrimSpace(resp)

	// Remove <think>...</think> blocks if present (common in reasoning models)
	// Handle both single line and multi-line think blocks
	if start := strings.Index(resp, "<think>"); start != -1 {
		if end := strings.Index(resp, "</think>"); end != -1 {
			// Keep only what's AFTER the closing tag
			resp = resp[end+8:]
		}
	}
	resp = strings.TrimSpace(resp)

	var decision decisionResponse
	if err := json.Unmarshal([]byte(resp), &decision); err != nil {
		// Fallback: try to parse just the queries if JSON fails, or just return error
		// For robustness, we could retry or use a simpler format.
		return decisionResponse{}, fmt.Errorf("failed to parse JSON decision: %w. Response was: %s", err, resp)
	}

	return decision, nil
}

// summarizePage uses LLM to create a short summary of a single page's content
func (a *DeepResearcher) summarizePage(url, title, content string) string {
	if len(content) < 100 {
		return content // Too short to summarize
	}
	
	prompt := fmt.Sprintf(`Summarize this webpage content in 2-3 sentences. Extract ONLY specific facts, prices, addresses, dates, or key data points. Be extremely concise.

Title: %s
URL: %s
Content:
%s

Summary (2-3 sentences, facts only):`, title, url, content)

	resp, err := a.llmClient.Chat([]llm.Message{
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return content[:min(len(content), 300)] // Fallback to truncated content
	}
	return stripThinkTags(resp)
}

func (a *DeepResearcher) parallelSearch(queries []string) string {
	var wg sync.WaitGroup
	var mu sync.Mutex // Mutex for thread-safe source collection
	resultsChan := make(chan string, len(queries))
	
	// Limit concurrency
	sem := make(chan struct{}, a.config.ParallelQuery)

	// Check if searcher supports content fetching and link extraction
	fetcher, canFetch := a.searcher.(search.ContentFetcher)
	linkExtractor, canExtract := a.searcher.(search.LinkExtractor)
	useDeepMode := a.config.DeepMode && canFetch

	for _, q := range queries {
		wg.Add(1)
		go func(query string) {
			defer wg.Done()
			sem <- struct{}{} // Acquire
			defer func() { <-sem }() // Release

			res, err := a.searcher.Search(query)
			if err != nil {
				resultsChan <- fmt.Sprintf("Error searching '%s': %v", query, err)
				return
			}

			if len(res) == 0 {
				resultsChan <- fmt.Sprintf("No results found for '%s'", query)
				return
			}

			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("Results for '%s':\n", query))
			
			if useDeepMode && canExtract {
				// DEEP MODE: Extract individual listing links from index pages, then fetch each
				fmt.Printf("   🔗 [DEEP] Extracting individual listings from search results...\n")
				
				listingsProcessed := 0
				maxListingsPerQuery := 5
				
				for _, r := range res {
					if listingsProcessed >= maxListingsPerQuery {
						break
					}
					
					// Extract listing links from this index page
					fmt.Printf("   📄 [DEEP] Extracting links from: %s\n", r.URL)
					links, err := linkExtractor.ExtractListingLinks(r.URL, 5)
					
					if err != nil || len(links) == 0 {
						// Fallback: treat this URL as a listing itself (might be a direct listing)
						fmt.Printf("   📄 [DEEP] No sub-links found, fetching page directly\n")
						if rawContent, err := fetcher.FetchPageContent(r.URL, 6000); err == nil && len(rawContent) > 50 {
							fmt.Printf("   🧠 [DEEP] Summarizing %d chars...\n", len(rawContent))
							summary := a.summarizePage(r.URL, r.Title, rawContent)
							sb.WriteString(fmt.Sprintf("- Title: %s\n  URL: %s\n  Details: %s\n", r.Title, r.URL, summary))
							
							mu.Lock()
							a.sources = append(a.sources, Source{Title: r.Title, URL: r.URL})
							mu.Unlock()
							listingsProcessed++
						}
						continue
					}
					
					// Process each individual listing
					for _, link := range links {
						if listingsProcessed >= maxListingsPerQuery {
							break
						}
						
						fmt.Printf("   🏠 [DEEP] Fetching listing: %s\n", link.URL)
						rawContent, err := fetcher.FetchPageContent(link.URL, 6000)
						if err != nil || len(rawContent) < 50 {
							continue
						}
						
						fmt.Printf("   🧠 [DEEP] Summarizing listing...\n")
						summary := a.summarizePage(link.URL, link.Title, rawContent)
						
						sb.WriteString(fmt.Sprintf("- LISTING: %s\n  URL: %s\n  Details: %s\n", link.Title, link.URL, summary))
						
						mu.Lock()
						a.sources = append(a.sources, Source{Title: link.Title, URL: link.URL})
						mu.Unlock()
						listingsProcessed++
					}
				}
				
				if listingsProcessed == 0 {
					sb.WriteString("  (No individual listings could be extracted)\n")
				}
				
			} else {
				// FAST MODE: Just use search snippets
				for i, r := range res {
					if i >= 5 { break }
					
					content := strings.ReplaceAll(r.Content, "\n", " ")
					sb.WriteString(fmt.Sprintf("- Title: %s\n  URL: %s\n  Summary: %s\n", r.Title, r.URL, content))
					
					mu.Lock()
					a.sources = append(a.sources, Source{Title: r.Title, URL: r.URL})
					mu.Unlock()
				}
			}
			
			resultsChan <- sb.String()
		}(q)
	}

	wg.Wait()
	close(resultsChan)

	var combinedResults strings.Builder
	for r := range resultsChan {
		combinedResults.WriteString(r)
		combinedResults.WriteString("\n")
	}
	
	if combinedResults.Len() == 0 {
		return "No search results found for any query."
	}

	return combinedResults.String()
}

func (a *DeepResearcher) summarize(topic, searchResults string) (string, error) {
	linkEmphasis := ""
	if a.config.ResultLinks {
		linkEmphasis = "\n\nCRITICAL: Extract and preserve ALL specific listing URLs (not category pages). Each item MUST have its own direct link in the format: [Title](URL)"
	}

	prompt := fmt.Sprintf(`Here are search results for the topic "%s":
%s

Extract and summarize SPECIFIC, CONCRETE information from these results:
- Extract exact prices, addresses, specifications, dates, names
- Include direct URLs to specific listings or pages (not just homepages)
- Quote specific data points when available
- If you see listings, extract: title, price, key details, and exact URL%s

Keep it dense and factual. Cite the exact URL for each piece of information.
Do not use <think> tags.
`, topic, searchResults, linkEmphasis)

	resp, err := a.llmClient.Chat([]llm.Message{
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return "", err
	}
	return stripThinkTags(resp), nil
}

func (a *DeepResearcher) writeReport(topic, context string) (string, error) {
	maxChars := a.config.maxContextChars()
	// Reserve ~40% of context for system prompt, topic, and response (more conservative)
	maxContextChars := int(float64(maxChars) * 0.5)
	
	// Retry loop with increasingly aggressive compression
	maxRetries := 3
	currentContext := context
	
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if len(currentContext) > maxContextChars {
			fmt.Printf("📦 Report attempt %d: context (%d chars) exceeds limit (%d), compressing...\n", 
				attempt, len(currentContext), maxContextChars)
			
			// Each retry compresses more aggressively
			targetRatio := 0.5 / float64(attempt) // 0.5, 0.25, 0.167
			compressed, err := a.compressContext(currentContext, targetRatio)
			if err != nil {
				fmt.Printf("⚠️ Compression attempt %d failed: %v\n", attempt, err)
				// Hard truncate as fallback
				if len(currentContext) > maxContextChars {
					currentContext = currentContext[:maxContextChars]
					fmt.Printf("   Hard truncated to %d chars\n", maxContextChars)
				}
			} else {
				currentContext = compressed
			}
		}
		
		// Try to generate the report
		linkEmphasis := ""
		if a.config.ResultLinks {
			linkEmphasis = "\n\nCRITICAL: Include direct clickable links [Title](URL) for each item."
		}

		prompt := fmt.Sprintf(`Write a research report for: %s

Data:
%s

Format with Markdown. Include source URLs.%s`, topic, currentContext, linkEmphasis)

		resp, err := a.llmClient.Chat([]llm.Message{
			{Role: "user", Content: prompt},
		})
		
		if err != nil {
			if attempt < maxRetries && (strings.Contains(err.Error(), "context") || strings.Contains(err.Error(), "token")) {
				fmt.Printf("⚠️ Report generation failed (attempt %d): %v\n", attempt, err)
				// Reduce context size more aggressively for next attempt
				maxContextChars = maxContextChars / 2
				continue
			}
			return "", fmt.Errorf("report generation failed after %d attempts: %w", attempt, err)
		}
		
		return stripThinkTags(resp), nil
	}
	
	return "", fmt.Errorf("failed to generate report after %d attempts", maxRetries)
}

// ========== EXHAUSTIVE MODE FUNCTIONS ==========

// normalizeURL normalizes a URL for deduplication (removes tracking params, trailing slashes)
func normalizeURL(rawURL string) string {
	// Remove common tracking parameters
	trackingParams := []string{"utm_source", "utm_medium", "utm_campaign", "utm_content", "utm_term", "fbclid", "gclid", "ref", "source"}
	
	u, err := url.Parse(rawURL)
	if err != nil {
		return strings.TrimSuffix(rawURL, "/")
	}
	
	q := u.Query()
	for _, param := range trackingParams {
		q.Del(param)
	}
	u.RawQuery = q.Encode()
	
	// Remove trailing slash
	u.Path = strings.TrimSuffix(u.Path, "/")
	
	return u.String()
}

// QueryExpansion holds LLM-generated expansion data for a topic
type QueryExpansion struct {
	Synonyms  map[string][]string `json:"synonyms"`  // word -> alternative words
	Platforms []string            `json:"platforms"` // relevant site: prefixes
}

// generateQueryExpansions uses LLM to generate domain-specific synonyms and platforms
func (a *DeepResearcher) generateQueryExpansions(topic string, baseQueries []string) (QueryExpansion, error) {
	prompt := fmt.Sprintf(`Analyze this research topic and base queries to generate search expansion data.

Topic: "%s"
Base queries: %v

Generate a JSON object with:
1. "synonyms": A map of key terms found in the queries to their synonyms/alternatives. Include:
   - Different words meaning the same thing
   - Abbreviations and full forms
   - Singular/plural forms if relevant
   - Industry-specific jargon alternatives
   - Translations if multiple languages are relevant

2. "platforms": A list of relevant "site:" prefixes for websites that would have this type of information. Think about:
   - What websites specialize in this topic?
   - What marketplaces, directories, or databases cover this?
   - What country/language-specific sites are relevant?
   - Include general platforms like social media if relevant

Examples:
- For real estate: platforms might be ["site:zillow.com", "site:realtor.com", "site:redfin.com"]
- For tech products: platforms might be ["site:amazon.com", "site:newegg.com", "site:bestbuy.com"]
- For academic research: platforms might be ["site:scholar.google.com", "site:arxiv.org", "site:researchgate.net"]
- For jobs: platforms might be ["site:linkedin.com", "site:indeed.com", "site:glassdoor.com"]

Respond ONLY with valid JSON:
{
  "synonyms": {
    "word1": ["alt1", "alt2", "alt3"],
    "word2": ["alt1", "alt2"]
  },
  "platforms": ["site:example1.com", "site:example2.com"]
}`, topic, baseQueries)

	resp, err := a.llmClient.Chat([]llm.Message{
		{Role: "system", Content: "You are a search optimization expert. Output only valid JSON. Be comprehensive with synonyms and platforms relevant to the specific topic and language."},
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return QueryExpansion{}, err
	}

	resp = stripThinkTags(resp)
	resp = strings.TrimPrefix(resp, "```json")
	resp = strings.TrimPrefix(resp, "```")
	resp = strings.TrimSuffix(resp, "```")
	resp = strings.TrimSpace(resp)

	var expansion QueryExpansion
	if err := json.Unmarshal([]byte(resp), &expansion); err != nil {
		// Return empty expansion on parse error - will just use base queries
		fmt.Printf("   ⚠️ Could not parse query expansions, using base queries only\n")
		return QueryExpansion{Synonyms: make(map[string][]string), Platforms: []string{}}, nil
	}

	return expansion, nil
}

// expandQueriesWithLLM generates diverse query variations using LLM-provided expansions
// Strategy: Keep queries SHORT. Don't combine site: with synonyms (causes explosion).
func expandQueriesWithLLM(baseQueries []string, expansion QueryExpansion) []string {
	expanded := make(map[string]bool) // Use map for dedup
	
	// 1. Add all base queries first (no prefix)
	for _, q := range baseQueries {
		if len(q) <= 60 { // Skip overly long queries
			expanded[q] = true
		}
	}
	
	// 2. Add base queries with platform prefixes (site: + original query)
	for _, q := range baseQueries {
		if len(q) > 40 { // Skip long queries for site: prefix
			continue
		}
		for _, platform := range expansion.Platforms {
			if platform != "" {
				newQuery := platform + " " + q
				expanded[newQuery] = true
			}
		}
	}
	
	// 3. Create synonym variations of base queries (WITHOUT site: prefix)
	// This avoids the explosion of site: + synonym combinations
	synonymQueries := make(map[string]bool)
	for _, q := range baseQueries {
		if len(q) > 50 { // Skip long queries
			continue
		}
		lowerQ := strings.ToLower(q)
		for word, syns := range expansion.Synonyms {
			wordLower := strings.ToLower(word)
			if strings.Contains(lowerQ, wordLower) {
				for _, syn := range syns {
					if strings.ToLower(syn) != wordLower {
						newQuery := strings.ReplaceAll(lowerQ, wordLower, strings.ToLower(syn))
						if len(newQuery) <= 60 {
							synonymQueries[newQuery] = true
						}
					}
				}
			}
		}
	}
	
	// Add synonym queries (no site: prefix)
	for q := range synonymQueries {
		expanded[q] = true
	}
	
	// 4. Cap total queries to avoid wasting time
	const maxQueries = 150
	result := make([]string, 0, len(expanded))
	for q := range expanded {
		result = append(result, q)
		if len(result) >= maxQueries {
			break
		}
	}
	
	return result
}

// CreatePlanExhaustive generates a research plan with pre-generated search queries
func (a *DeepResearcher) CreatePlanExhaustive(topic string, additionalContext string) (ResearchPlan, error) {
	contextInfo := ""
	if additionalContext != "" {
		contextInfo = fmt.Sprintf("\n\nAdditional context from user:\n%s", additionalContext)
	}

	prompt := fmt.Sprintf(`You are a Deep Research AI planning an EXHAUSTIVE data collection task.

User's research request: "%s"%s

Your goal is to find AS MANY results as possible. Generate a research plan focused on comprehensive coverage.

Output a JSON object with:
1. "clarifying_questions": List of 2-3 questions about the search criteria
2. "understanding_summary": A 1-2 sentence summary of what data to collect
3. "research_steps": A list of 3-5 specific research steps
4. "expected_outcome": What the final data collection will contain
5. "search_queries": A list of 15-25 SHORT, SIMPLE search queries. CRITICAL RULES:
   - Each query must be 2-5 words MAXIMUM
   - Use simple keyword combinations, NOT full sentences
   - DO NOT include numbers, prices, sizes, or complex filters
   - DO NOT include "site:" prefixes
   - Include variations: different word orders, singular/plural, abbreviations
   - Use the language appropriate for the topic

Respond ONLY with valid JSON:
{
  "clarifying_questions": ["question1", "question2"],
  "understanding_summary": "...",
  "research_steps": ["step1", "step2", "step3"],
  "expected_outcome": "...",
  "search_queries": ["short query 1", "short query 2", ...]
}`, topic, contextInfo)

	resp, err := a.llmClient.Chat([]llm.Message{
		{Role: "system", Content: "You are a research planning assistant. Output only valid JSON. Focus on generating diverse, comprehensive search queries without site: prefixes."},
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return ResearchPlan{}, err
	}

	resp = stripThinkTags(resp)
	resp = strings.TrimPrefix(resp, "```json")
	resp = strings.TrimPrefix(resp, "```")
	resp = strings.TrimSuffix(resp, "```")
	resp = strings.TrimSpace(resp)

	var plan ResearchPlan
	if err := json.Unmarshal([]byte(resp), &plan); err != nil {
		return ResearchPlan{}, fmt.Errorf("failed to parse research plan: %w. Response: %s", err, resp)
	}

	// Use LLM to generate domain-specific expansions
	if len(plan.SearchQueries) > 0 {
		fmt.Printf("🔍 Generating query expansions for topic...\n")
		expansion, err := a.generateQueryExpansions(topic, plan.SearchQueries)
		if err != nil {
			fmt.Printf("   ⚠️ Could not generate expansions: %v\n", err)
			// Continue with base queries only
		} else {
			if len(expansion.Platforms) > 0 {
				fmt.Printf("   📡 Found %d relevant platforms\n", len(expansion.Platforms))
			}
			if len(expansion.Synonyms) > 0 {
				fmt.Printf("   📝 Found synonyms for %d terms\n", len(expansion.Synonyms))
			}
			plan.SearchQueries = expandQueriesWithLLM(plan.SearchQueries, expansion)
		}
		fmt.Printf("📋 Expanded to %d search queries\n", len(plan.SearchQueries))
	}

	return plan, nil
}

// RunExhaustive executes exhaustive research mode
// - Ignores LLM "final_answer" decision
// - Uses pre-generated queries from plan
// - Paginates through search results
// - Deduplicates URLs
// - Shows live progress
func (a *DeepResearcher) RunExhaustive(topic string, plan ResearchPlan) (ResearchResult, error) {
	// Reset state
	a.mu.Lock()
	a.sources = make([]Source, 0)
	a.seenURLs = make(map[string]bool)
	a.mu.Unlock()

	if len(plan.SearchQueries) == 0 {
		return ResearchResult{}, fmt.Errorf("no search queries in plan - use CreatePlanExhaustive")
	}

	fmt.Printf("\n🔥 Starting Exhaustive Research for: %s\n", topic)
	pagesDesc := "auto (until empty)"
	if a.config.MaxPages > 0 {
		pagesDesc = fmt.Sprintf("%d", a.config.MaxPages)
	}
	fmt.Printf("📋 Processing %d search queries, pages: %s\n", len(plan.SearchQueries), pagesDesc)
	fmt.Printf("🎯 Target: %d unique results | ⏱️ Delay: %dms between requests\n\n", a.config.MinResults, a.config.DelayMs)

	// Build initial context
	researchContext := fmt.Sprintf(`User Query: %s

Research Plan:
- Understanding: %s
- Expected Outcome: %s

Knowledge gathered:
`, topic, plan.UnderstandingSummary, plan.ExpectedOutcome)

	queriesPerRound := a.config.ParallelQuery
	totalQueries := len(plan.SearchQueries)
	queryIndex := 0
	
	// Stats tracking
	totalURLsFound := 0
	totalDuplicates := 0

	for round := 0; round < a.config.MaxLoops && queryIndex < totalQueries; round++ {
		fmt.Printf("=== Round %d/%d ===\n", round+1, a.config.MaxLoops)

		// Get queries for this round
		endIndex := queryIndex + queriesPerRound
		if endIndex > totalQueries {
			endIndex = totalQueries
		}
		roundQueries := plan.SearchQueries[queryIndex:endIndex]
		queryIndex = endIndex

		fmt.Printf("🔎 Processing queries %d-%d of %d\n", queryIndex-len(roundQueries)+1, queryIndex, totalQueries)

		// Process queries with pagination
		roundResults, newURLs, duplicates := a.searchWithPagination(roundQueries)
		totalURLsFound += newURLs
		totalDuplicates += duplicates

		if roundResults != "" {
			researchContext += fmt.Sprintf("\n--- Round %d Results ---\n%s", round+1, roundResults)
		}

		// Context compression check: compress when context exceeds 50% of max capacity
		maxChars := a.config.maxContextChars()
		compressionThreshold := int(float64(maxChars) * 0.5)
		if len(researchContext) > compressionThreshold {
			fmt.Printf("📦 Context size (%d chars) exceeds threshold (%d), compressing...\n", 
				len(researchContext), compressionThreshold)
			compressed, err := a.compressContext(researchContext, 0.5)
			if err != nil {
				fmt.Printf("⚠️ Context compression failed: %v (continuing with full context)\n", err)
			} else {
				researchContext = compressed
			}
		}

		// Check if we've hit the minimum
		a.mu.Lock()
		currentUniqueCount := len(a.sources)
		a.mu.Unlock()

		fmt.Printf("📊 Round %d complete: %d new URLs, %d duplicates skipped\n", round+1, newURLs, duplicates)
		fmt.Printf("📈 Total progress: %d unique listings", currentUniqueCount)
		
		if currentUniqueCount >= a.config.MinResults {
			fmt.Printf(" ✅ Target reached!\n\n")
			fmt.Printf("🎯 Stopping early: found %d unique listings (target: %d)\n", currentUniqueCount, a.config.MinResults)
			break
		}
		fmt.Printf(" (target: %d)\n\n", a.config.MinResults)
	}

	// Final stats
	a.mu.Lock()
	finalCount := len(a.sources)
	a.mu.Unlock()

	fmt.Printf("\n📊 Final stats: %d unique URLs collected, %d duplicates skipped\n", finalCount, totalDuplicates)

	// Write report
	fmt.Println("\n✍️ Writing Final Report...")
	report, err := a.writeReport(topic, researchContext)
	if err != nil {
		return ResearchResult{}, err
	}

	a.mu.Lock()
	sources := make([]Source, len(a.sources))
	copy(sources, a.sources)
	a.mu.Unlock()

	return ResearchResult{Report: report, Sources: sources}, nil
}

// searchWithPagination searches queries across multiple pages with rate limiting
func (a *DeepResearcher) searchWithPagination(queries []string) (string, int, int) {
	var results strings.Builder
	newURLs := 0
	duplicates := 0

	// Check if searcher supports pagination
	type paginatedSearcher interface {
		SearchWithPage(query string, page int) ([]search.Result, error)
	}
	pagSearcher, canPaginate := a.searcher.(paginatedSearcher)
	
	// Check if we can fetch content
	fetcher, canFetch := a.searcher.(search.ContentFetcher)
	useDeepMode := a.config.DeepMode && canFetch

	for _, query := range queries {
		// Determine max pages: 0 means auto (keep going until empty), otherwise use configured value
		maxPages := a.config.MaxPages
		if maxPages == 0 {
			maxPages = 100 // Safety limit for auto-pagination
		}
		
		for page := 1; page <= maxPages; page++ {
			// Rate limiting delay
			if a.config.DelayMs > 0 {
				time.Sleep(time.Duration(a.config.DelayMs) * time.Millisecond)
			}

			var searchResults []search.Result
			var err error
			
			if canPaginate {
				searchResults, err = pagSearcher.SearchWithPage(query, page)
			} else {
				if page == 1 {
					searchResults, err = a.searcher.Search(query)
				} else {
					break // Skip pagination if not supported
				}
			}

			if err != nil {
				fmt.Printf("   ❌ Error searching '%s' (page %d): %v\n", query, page, err)
				break // Stop this query on error
			}

			if len(searchResults) == 0 {
				if page == 1 {
					fmt.Printf("   [%s] page %d → 0 results\n", truncateQuery(query, 40), page)
				}
				break // No more results for this query
			}

			fmt.Printf("   [%s] page %d → %d results\n", truncateQuery(query, 40), page, len(searchResults))

			// Process results
			for _, r := range searchResults {
				normalizedURL := normalizeURL(r.URL)
				
				a.mu.Lock()
				if a.seenURLs[normalizedURL] {
					a.mu.Unlock()
					duplicates++
					continue
				}
				a.seenURLs[normalizedURL] = true
				a.mu.Unlock()

				newURLs++

				// Add to results
				if useDeepMode {
					// Fetch and summarize page content
					if a.config.DelayMs > 0 {
						time.Sleep(time.Duration(a.config.DelayMs) * time.Millisecond)
					}
					content, err := fetcher.FetchPageContent(r.URL, 6000)
					if err == nil && len(content) > 50 {
						summary := a.summarizePage(r.URL, r.Title, content)
						results.WriteString(fmt.Sprintf("- LISTING: %s\n  URL: %s\n  Details: %s\n\n", r.Title, r.URL, summary))
					} else {
						results.WriteString(fmt.Sprintf("- %s\n  URL: %s\n  Snippet: %s\n\n", r.Title, r.URL, r.Content))
					}
				} else {
					results.WriteString(fmt.Sprintf("- %s\n  URL: %s\n  Snippet: %s\n\n", r.Title, r.URL, r.Content))
				}

				// Track source
				a.mu.Lock()
				a.sources = append(a.sources, Source{Title: r.Title, URL: r.URL})
				a.mu.Unlock()
			}
		}
	}

	return results.String(), newURLs, duplicates
}

// truncateQuery truncates a query for display
func truncateQuery(q string, maxLen int) string {
	if len(q) <= maxLen {
		return q
	}
	return q[:maxLen-3] + "..."
}
