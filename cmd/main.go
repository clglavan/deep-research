package main

import (
	"bufio"
	"deep-research/pkg/agent"
	"deep-research/pkg/llm"
	"deep-research/pkg/search"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func getWSLHostIP() string {
	// Method 1: Check 'ip route' for the default gateway (Most reliable)
	cmd := exec.Command("ip", "route", "show", "default")
	output, err := cmd.Output()
	if err == nil {
		// Output format: "default via 172.x.x.x dev eth0 ..."
		fields := strings.Fields(string(output))
		if len(fields) >= 3 && fields[0] == "default" && fields[1] == "via" {
			return fields[2]
		}
	}

	// Method 2: Fallback to /etc/resolv.conf
	file, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return "localhost"
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "nameserver") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[1]
			}
		}
	}
	return "localhost"
}

func main() {
	defaultLMURL := "http://localhost:1234/v1"
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		hostIP := getWSLHostIP()
		defaultLMURL = fmt.Sprintf("http://%s:1234/v1", hostIP)
		fmt.Printf("🐧 Detected WSL. Defaulting LM Studio URL to host: %s\n", defaultLMURL)
		fmt.Println("⚠️  Ensure LM Studio is listening on 0.0.0.0 (Settings -> Local Server -> Network Support)")
	}

	lmURL := flag.String("lm-url", defaultLMURL, "LM Studio Base URL")
	searxURL := flag.String("searx-url", "http://localhost:8080", "SearXNG Base URL")
	model := flag.String("model", "local-model", "Model name (optional for LM Studio)")
	maxLoops := flag.Int("loops", 5, "Max research loops (default: 5)")
	parallel := flag.Int("parallel", 5, "Max parallel searches (default: 5)")
	useMock := flag.Bool("mock", false, "Use mock search (for testing without SearXNG)")
	outputFile := flag.String("o", "", "Output file path (default: results/<timestamp>_<topic>.md)")
	contextLen := flag.Int("ctx", 32768, "Context length for LLM (default: 32768)")
	deepMode := flag.Bool("deep", false, "Deep mode: fetch and summarize each page (slower but more thorough)")
	resultLinks := flag.Bool("result-links", false, "Emphasize including direct links to individual listings in results")
	
	// Simple mode flag (exhaustive is now the default)
	simpleMode := flag.Bool("simple", false, "Simple mode: quick research without query expansion (not recommended)")
	minResults := flag.Int("min-results", 20, "Minimum unique URLs to find before stopping")
	delayMs := flag.Int("delay", 500, "Milliseconds delay between HTTP requests (rate limiting)")
	maxPages := flag.Int("pages", 0, "Max pages per query (0 = auto: keep fetching until no more results)")
	
	// Non-interactive mode flags
	topicFlag := flag.String("topic", "", "Research topic (skips interactive prompt)")
	autoApprove := flag.Bool("yes", false, "Auto-approve research plan without confirmation (use with --topic)")
	flag.Parse()

	if *deepMode {
		fmt.Println("🔬 Deep mode enabled: will fetch and summarize each page individually")
	}
	if *resultLinks {
		fmt.Println("🔗 Result links mode: will emphasize direct listing URLs in output")
	}
	if *simpleMode {
		fmt.Println("⚡ Simple mode: quick research without query expansion (less thorough)")
	} else {
		fmt.Println("🔥 Exhaustive mode (default): pre-generating queries, forcing all loops, deduplicating URLs")
		pagesDesc := "auto (until empty)"
		if *maxPages > 0 {
			pagesDesc = fmt.Sprintf("%d", *maxPages)
		}
		fmt.Printf("   Min results: %d | Delay: %dms | Pages per query: %s\n", *minResults, *delayMs, pagesDesc)
	}

	// 1. Setup LLM
	llmClient := llm.NewClient(llm.Config{
		BaseURL:       *lmURL,
		APIKey:        "lm-studio",
		Model:         *model,
		Temperature:   0.0,
		ContextLength: *contextLen,
		Timeout:       5 * time.Minute, // Long timeout for reasoning
	})

	// 2. Setup Search
	var searcher search.Searcher
	if *useMock {
		fmt.Println("⚠️ Using Mock Search Engine")
		searcher = &search.MockClient{}
	} else {
		fmt.Printf("🔎 Using SearXNG at %s\n", *searxURL)
		searcher = search.NewSearXNGClient(*searxURL)
	}

	// 3. Setup Agent
	researcher := agent.NewDeepResearcher(llmClient, searcher, agent.Config{
		MaxLoops:      *maxLoops,
		ParallelQuery: *parallel,
		DeepMode:      *deepMode,
		ResultLinks:   *resultLinks,
		SimpleMode:    *simpleMode,
		MinResults:    *minResults,
		DelayMs:       *delayMs,
		MaxPages:      *maxPages,
		ContextLength: *contextLen,
	})

	// 4. Get Input
	reader := bufio.NewReader(os.Stdin)
	var topic string
	
	if *topicFlag != "" {
		topic = *topicFlag
		fmt.Printf("\n🧪 Research topic: %s\n", topic)
	} else {
		fmt.Print("\n🧪 Enter research topic: ")
		topic, _ = reader.ReadString('\n')
		topic = strings.TrimSpace(topic)
	}

	if topic == "" {
		fmt.Println("Please enter a topic.")
		return
	}

	// 5. Planning Phase - Interactive Loop
	var plan agent.ResearchPlan
	additionalContext := ""
	
	for {
		fmt.Println("\n📋 Creating research plan...")
		var err error
		
		// Use simple plan generator only if --simple flag is set
		// Exhaustive (with query expansion) is the default
		if *simpleMode {
			plan, err = researcher.CreatePlan(topic, additionalContext)
		} else {
			plan, err = researcher.CreatePlanExhaustive(topic, additionalContext)
		}
		if err != nil {
			fmt.Printf("\n❌ Error creating plan: %v\n", err)
			return
		}

		// Display the plan
		fmt.Println("\n" + strings.Repeat("─", 50))
		fmt.Println("📝 RESEARCH PLAN")
		fmt.Println(strings.Repeat("─", 50))
		
		fmt.Printf("\n🎯 Understanding: %s\n", plan.UnderstandingSummary)
		
		if len(plan.ClarifyingQuestions) > 0 {
			fmt.Println("\n❓ Clarifying Questions:")
			for i, q := range plan.ClarifyingQuestions {
				fmt.Printf("   %d. %s\n", i+1, q)
			}
		}
		
		fmt.Println("\n📌 Research Steps:")
		for i, step := range plan.ResearchSteps {
			fmt.Printf("   %d. %s\n", i+1, step)
		}
		
		fmt.Printf("\n📊 Expected Outcome: %s\n", plan.ExpectedOutcome)
		
		// Show search queries (unless in simple mode)
		if !*simpleMode && len(plan.SearchQueries) > 0 {
			fmt.Printf("\n🔎 Search Queries (%d total):\n", len(plan.SearchQueries))
			displayCount := 10
			if len(plan.SearchQueries) < displayCount {
				displayCount = len(plan.SearchQueries)
			}
			for i := 0; i < displayCount; i++ {
				fmt.Printf("   %d. %s\n", i+1, plan.SearchQueries[i])
			}
			if len(plan.SearchQueries) > displayCount {
				fmt.Printf("   ... and %d more queries\n", len(plan.SearchQueries)-displayCount)
			}
		}
		
		fmt.Println(strings.Repeat("─", 50))

		// Auto-approve if --yes flag is set
		if *autoApprove {
			fmt.Println("\n✅ Plan auto-approved (--yes flag)! Starting research...")
			break
		}

		// Ask for approval
		fmt.Println("\nOptions:")
		fmt.Println("  [Enter]  - Approve and start research")
		fmt.Println("  [r]      - Revise plan (provide more details)")
		fmt.Println("  [q]      - Quit")
		fmt.Print("\nYour choice: ")
		
		choice, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(strings.ToLower(choice))

		if choice == "" {
			fmt.Println("\n✅ Plan approved! Starting research...")
			break
		} else if choice == "q" {
			fmt.Println("Research cancelled.")
			return
		} else if choice == "r" {
			fmt.Print("\n📝 Enter additional details or answer the questions above:\n> ")
			additionalContext, _ = reader.ReadString('\n')
			additionalContext = strings.TrimSpace(additionalContext)
			continue
		} else {
			// Treat any other input as additional context
			additionalContext = choice
			continue
		}
	}

	// 6. Execute Research
	start := time.Now()
	var result agent.ResearchResult
	var err error
	
	// Use simple Run only if --simple flag is set
	// RunExhaustive is the default
	if *simpleMode {
		result, err = researcher.Run(topic, plan)
	} else {
		result, err = researcher.RunExhaustive(topic, plan)
	}
	if err != nil {
		fmt.Printf("\n❌ Error: %v\n", err)
		return
	}

	// 7. Build final output with bibliography
	var finalOutput strings.Builder
	finalOutput.WriteString(result.Report)
	finalOutput.WriteString("\n\n---\n\n## Bibliography\n\n")
	
	// Deduplicate sources
	seen := make(map[string]bool)
	for i, src := range result.Sources {
		if !seen[src.URL] {
			seen[src.URL] = true
			finalOutput.WriteString(fmt.Sprintf("%d. [%s](%s)\n", i+1, src.Title, src.URL))
		}
	}

	// 7. Determine output file path
	outPath := *outputFile
	if outPath == "" {
		// Create results directory
		if err := os.MkdirAll("results", 0755); err != nil {
			fmt.Printf("⚠️ Could not create results directory: %v\n", err)
		}
		// Generate filename from topic
		safeTopic := sanitizeFilename(topic)
		if len(safeTopic) > 50 {
			safeTopic = safeTopic[:50]
		}
		outPath = filepath.Join("results", fmt.Sprintf("%s_%s.md", time.Now().Format("20060102_150405"), safeTopic))
	}

	// 8. Write to file
	if err := os.WriteFile(outPath, []byte(finalOutput.String()), 0644); err != nil {
		fmt.Printf("⚠️ Could not write to file: %v\n", err)
	} else {
		fmt.Printf("\n📄 Report saved to: %s\n", outPath)
	}

	// 9. Print to console
	fmt.Printf("\n\n%s\n", strings.Repeat("=", 50))
	fmt.Println(finalOutput.String())
	fmt.Printf("%s\n", strings.Repeat("=", 50))
	fmt.Printf("⏱️ Completed in %v\n", time.Since(start))
}

// sanitizeFilename removes or replaces characters that are not safe for filenames
func sanitizeFilename(s string) string {
	// Replace spaces with underscores
	s = strings.ReplaceAll(s, " ", "_")
	// Remove any character that's not alphanumeric, underscore, or hyphen
	reg := regexp.MustCompile(`[^a-zA-Z0-9_-]`)
	s = reg.ReplaceAllString(s, "")
	return strings.ToLower(s)
}
