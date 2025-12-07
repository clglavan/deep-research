# Local Deep Research Agent (Go)

A high-performance, parallelized Deep Research agent written in Go. It connects your local LLM (via LM Studio) with a local search engine (SearXNG) to perform iterative, deep research on any topic.

## Architecture

- **Brain**: Local LLM (via LM Studio)
- **Eyes**: SearXNG (Local Meta-Search Engine)
- **Agent**: Go application (Plan -> Search -> Summarize Loop)

## Prerequisites

1.  **LM Studio**: Running a local server.
    - Load a model (recommended: `qwen/qwen3-4b-thinking-2507` - reliable and fast).
    - Start Server.
    - **WSL Users**: Ensure "Network Support" (Listen on all interfaces / 0.0.0.0) is ENABLED in LM Studio settings so WSL can see it.
    - Ensure Context Length is high (e.g., 8192+).

2.  **SearXNG** (Required for real search):
    - Run via Docker with the included settings file:
      ```bash
      docker run -d -p 8080:8080 -v $(pwd)/searxng-settings.yml:/etc/searxng/settings.yml:ro searxng/searxng
      ```
    - The `searxng-settings.yml` file enables JSON API output which is required for this tool.

## Installation

```bash
go build -o deep-research cmd/main.go
```

## Usage

### 1. With SearXNG (Real Research)
Ensure SearXNG is running on port 8080.

```bash
./deep-research
```

Or specify URLs:
```bash
./deep-research -lm-url="http://localhost:1234/v1" -searx-url="http://localhost:8080"
```

### 2. Mock Mode (Testing without Search)
If you don't have SearXNG running yet, you can test the agent loop with mock data:

```bash
./deep-research -mock
```

## How It Works

This agent performs **iterative deep research** by combining an LLM's reasoning capabilities with web search. Here's the detailed flow:

### Phase 1: Planning

1. **Topic Input**: You provide a research topic (interactively or via `--topic` flag).

2. **Query Generation**: The LLM analyzes your topic and generates:
   - A summary of what it understands you want
   - Clarifying questions (displayed for context)
   - Research steps it plans to follow
   - **15-25 short search queries** (2-5 words each) to find relevant information

3. **Query Expansion** (Exhaustive Mode - default):
   - The LLM identifies relevant **platforms** for your topic (e.g., specialized websites, forums, databases)
   - It generates **synonyms** for key terms in your queries
   - Queries are expanded by combining base queries with `site:` prefixes and synonym variations
   - This typically expands 15-25 base queries into **50-150 diverse queries**
   - *Skip this with `--simple` flag for faster but less thorough research*

4. **Plan Approval**: You review the plan and can approve, revise, or quit. Use `--yes` to auto-approve.

### Phase 2: Research Execution

The agent processes queries in **rounds** (controlled by `--loops`):

```
┌─────────────────────────────────────────────────────────────────┐
│  ROUND 1                                                        │
│  ┌─────────────┐    ┌─────────────┐    ┌─────────────┐         │
│  │  Query 1    │    │  Query 2    │    │  Query N    │         │
│  │  (parallel) │    │  (parallel) │    │  (parallel) │  ←──────┼── --parallel controls batch size
│  └──────┬──────┘    └──────┬──────┘    └──────┬──────┘         │
│         │                  │                  │                 │
│         ▼                  ▼                  ▼                 │
│  ┌─────────────────────────────────────────────────────┐       │
│  │  SearXNG Search (with pagination if --pages > 0)    │       │
│  │  Each query can fetch multiple pages of results     │       │
│  └─────────────────────────────────────────────────────┘       │
│         │                                                       │
│         ▼                                                       │
│  ┌─────────────────────────────────────────────────────┐       │
│  │  URL Deduplication (skip already-seen URLs)         │       │
│  └─────────────────────────────────────────────────────┘       │
│         │                                                       │
│         ▼  (if --deep mode)                                    │
│  ┌─────────────────────────────────────────────────────┐       │
│  │  Fetch & Summarize each page via LLM                │       │
│  └─────────────────────────────────────────────────────┘       │
│         │                                                       │
│         ▼                                                       │
│  ┌─────────────────────────────────────────────────────┐       │
│  │  Accumulate results into Research Context           │       │
│  └─────────────────────────────────────────────────────┘       │
│         │                                                       │
│         ▼  (if context > 50% of --ctx limit)                   │
│  ┌─────────────────────────────────────────────────────┐       │
│  │  LLM Compression: summarize context to ~50%         │       │
│  │  Preserves: URLs, prices, names, specific data      │       │
│  │  Removes: redundancy, verbose descriptions          │       │
│  └─────────────────────────────────────────────────────┘       │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
              Check: Found >= --min-results URLs?
                     OR processed all queries?
                     OR completed --loops rounds?
                              │
                      ┌───────┴───────┐
                      │ NO            │ YES
                      ▼               ▼
               Continue to      Proceed to
               next round       Report Phase
```

### Phase 3: Report Generation

1. **Final Context Compression**: If the accumulated context exceeds 70% of the LLM's context window (`--ctx`), it's compressed to fit.

2. **Report Writing**: The LLM generates a comprehensive Markdown report based on all gathered information, including:
   - Summary of findings
   - Detailed analysis
   - Direct links to sources (especially with `--result-links`)
   - Bibliography of all URLs visited

3. **Output**: Report is saved to `results/` directory (or custom path via `-o`).

### Key Concepts

| Concept | Description |
|---------|-------------|
| **Exhaustive Mode** | Default. Pre-generates diverse queries, forces all loops to run, deduplicates URLs. More thorough. |
| **Simple Mode** | (`--simple`) LLM decides when to stop, generates queries on-the-fly. Faster but may miss results. |
| **Deep Mode** | (`--deep`) Fetches full page content and summarizes each result. Much slower but extracts detailed info. |
| **Context Compression** | Automatically compresses research context when it grows too large, preserving essential data. |
| **Rate Limiting** | (`--delay`) Prevents overwhelming search engines. Default 500ms between requests. |
| **Pagination** | (`--pages`) Fetches multiple pages of search results per query. `0` = auto (until empty). |

## Configuration Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-topic` | *(interactive)* | Research topic. If provided, skips the interactive prompt. Use with `-yes` for fully automated runs. |
| `-yes` | `false` | Auto-approve the research plan without confirmation. Useful for scripting/automation. |
| `-loops` | `5` | Maximum number of research rounds. Each round processes a batch of queries. Higher = more thorough but slower. |
| `-parallel` | `5` | Number of queries to process in parallel per round. Higher = faster but more load on SearXNG. |
| `-ctx` | `32768` | LLM context length in tokens. Must match your model's context size. Used for automatic context compression. |
| `-deep` | `false` | Deep mode: fetches and summarizes each result page individually. Much slower but extracts more detailed information. |
| `-result-links` | `false` | Emphasizes finding direct links to individual items/listings in the final report. |
| `-min-results` | `20` | Minimum unique URLs to collect before stopping early. Research continues until this target or max loops reached. |
| `-delay` | `500` | Milliseconds delay between HTTP requests. Rate limiting to avoid overwhelming search engines. |
| `-pages` | `0` | Max result pages to fetch per query. `0` = auto (keeps fetching until no more results). |
| `-simple` | `false` | Simple mode: disables query expansion. Faster but less thorough. Not recommended for comprehensive research. |
| `-o` | `results/<timestamp>_<topic>.md` | Output file path for the research report. |
| `-lm-url` | `http://localhost:1234/v1` (or WSL host) | LM Studio API endpoint. Auto-detects WSL and uses host IP. |
| `-searx-url` | `http://localhost:8080` | SearXNG instance URL. |
| `-model` | `local-model` | Model name sent to LLM API. LM Studio ignores this (uses loaded model), but other APIs may use it. |
| `-mock` | `false` | Use mock search results for testing without SearXNG running. |

### Example Commands

```bash
# Interactive mode (prompts for topic)
./deep-research

# Fully automated research
./deep-research --topic "best practices for Go error handling" --yes

# Thorough research with deep mode
./deep-research --topic "AI startups 2024" --yes --deep --loops 10 --min-results 50

# Fast research with limited scope  
./deep-research --topic "golang context package" --yes --loops 3 --simple

# Custom output file
./deep-research --topic "kubernetes networking" --yes -o ./my-research.md
```

## Context Management & Compression

### The Problem

When researching comprehensively, the agent can accumulate hundreds of search results. For example, 575 URLs with snippets can produce 200,000+ characters (~80,000 tokens) of context. Most local models have context limits of 4K-32K tokens, causing overflow errors.

### The Solution: Adaptive Chunked Compression

The agent implements a multi-layer compression system that automatically handles context overflow:

```
Large Context (e.g., 200,000 chars)
            │
            ▼
┌─────────────────────────────────────┐
│  1. Check: Fits in model context?   │
│     If YES → use directly           │
│     If NO  → proceed to chunking    │
└─────────────────────────────────────┘
            │
            ▼
┌─────────────────────────────────────┐
│  2. Split into chunks that fit      │
│     (~50% of model's context each)  │
│     Break on paragraph boundaries   │
└─────────────────────────────────────┘
            │
            ▼
┌─────────────────────────────────────┐
│  3. Compress each chunk via LLM     │
│     Preserve: URLs, prices, names,  │
│     numbers, dates, specific facts  │
│     Remove: redundancy, verbose text│
└─────────────────────────────────────┘
            │
            ▼
┌─────────────────────────────────────┐
│  4. Combine compressed chunks       │
│     Still too large? Recurse to #2  │
└─────────────────────────────────────┘
            │
            ▼
┌─────────────────────────────────────┐
│  5. Report Generation with Retries  │
│     Attempt 1: 50% compression      │
│     Attempt 2: 25% compression      │
│     Attempt 3: 16.7% compression    │
│     Final fallback: hard truncation │
└─────────────────────────────────────┘
```

### What Gets Preserved vs Removed

| Preserved (Essential Data) | Removed (Redundant) |
|---------------------------|---------------------|
| URLs and direct links | Verbose descriptions |
| Prices and numbers | Repeated information |
| Names and addresses | Meta-commentary |
| Dates and timestamps | Filler text |
| Specific facts and quotes | Navigation text |

## Context Length Guidelines

**⚠️ IMPORTANT:** The `--ctx` flag must match your model's actual context length in LM Studio.

### Recommended Settings by Model Size

| Model Context | `--ctx` | `--loops` | `--parallel` | `--min-results` | Notes |
|---------------|---------|-----------|--------------|-----------------|-------|
| 4K tokens | `4096` | `2-3` | `3` | `10` | Very limited - use `--simple` mode |
| 8K tokens | `8192` | `3-5` | `5` | `20` | Moderate research, compression will activate |
| 16K tokens | `16384` | `5-7` | `7` | `30` | Good balance for most research |
| 32K tokens | `32768` | `7-10` | `10` | `50` | Comprehensive research |
| 64K+ tokens | `65536` | `10+` | `10` | `100+` | Extensive deep research |

### Stable Configuration Examples

**For 8K context models (e.g., Qwen 4B):**
```bash
./deep-research --ctx 8192 --loops 3 --parallel 5 --min-results 15 --topic "your topic" --yes
```

**For 32K context models:**
```bash
./deep-research --ctx 32768 --loops 7 --parallel 10 --min-results 50 --topic "your topic" --yes
```

### Signs of Context Overflow

If you see these messages, your context is too small for the research scope:

```
📦 Context too large for single compression, using chunked approach...
📦 Split into 15 chunks for compression
⚠️ Report generation failed (attempt 1): context overflow
```

**Solutions:**
1. **Increase model context** in LM Studio and match with `--ctx`
2. **Reduce research scope** with lower `--loops`, `--parallel`, `--min-results`
3. **Use `--simple` mode** for faster, less comprehensive research
4. **Use a larger model** with more context capacity

### Why Larger Context Matters

| Context Size | Research Capability |
|--------------|---------------------|
| Small (4-8K) | Can only hold ~20-50 search results. Heavy compression loses detail. Best for simple queries. |
| Medium (16-32K) | Holds ~100-300 results. Moderate compression. Good for most research tasks. |
| Large (64K+) | Holds 500+ results with minimal compression. Ideal for comprehensive deep research. |

**Bottom line:** For serious research, use a model with at least 16K context. The `qwen/qwen3-4b-thinking-2507` model with 8K context works well for moderate research with the compression system, but larger models will produce more detailed reports.
