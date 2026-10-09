package builtin

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"ultrac/internal/contract/tool"
)

// WebSearchInput mirrors WebSearchTool's schema.
type WebSearchInput struct {
	Query          string   `json:"query"`
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	BlockedDomains []string `json:"blocked_domains,omitempty"`
}

const webSearchSchema = `{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "minLength": 2,
      "description": "The search query to use"
    },
    "allowed_domains": {
      "type": "array",
      "items": { "type": "string" },
      "description": "Only include search results from these domains"
    },
    "blocked_domains": {
      "type": "array",
      "items": { "type": "string" },
      "description": "Never include search results from these domains"
    }
  },
  "required": ["query"],
  "additionalProperties": false
}`

const webSearchDescription = `- Allows Claude to search the web and use the results to inform responses
- Provides up-to-date information for current events and recent data
- Returns search result information formatted as search result blocks
- Use this tool for accessing information beyond Claude's knowledge cutoff
- Searches are performed automatically within a single API call

NOTE: this engine has no hosted search API. It queries DuckDuckGo's HTML
endpoint, which is unauthenticated and can rate-limit or block automated
clients. When it is unavailable the tool says so rather than returning an empty
result set, because "no results" and "the search did not run" must not look the
same to you.`

const maxSearchResults = 10

var (
	reResultLink = regexp.MustCompile(`(?s)<a[^>]+class="[^"]*result__a[^"]*"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	reResultSnip = regexp.MustCompile(`(?s)class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>`)
)

type webSearchTool struct {
	spec[WebSearchInput]
}

func (webSearchTool) Call(ctx context.Context, in WebSearchInput, _ *tool.UseContext, _ tool.CanUseFunc, _ tool.ProgressFunc) (*tool.Result, error) {
	query := strings.TrimSpace(in.Query)
	if len(query) < 2 {
		return errResult("websearch: query must be at least 2 characters"), nil
	}

	endpoint := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	body, _, err := doFetch(ctx, endpoint, "text/html")
	if err != nil {
		return errResult(
			"websearch: the search endpoint is unavailable (%v). "+
				"No results were retrieved — this is not the same as finding nothing. "+
				"If you know a URL, use WebFetch instead.", err), nil
	}
	html := string(body)

	links := reResultLink.FindAllStringSubmatch(html, -1)
	snippets := reResultSnip.FindAllStringSubmatch(html, -1)

	var b strings.Builder
	b.WriteString("Search results for: " + query + "\n\n")
	found := 0
	for i, m := range links {
		if found >= maxSearchResults {
			break
		}
		href := decodeDDGLink(m[1])
		title := cleanFragment(m[2])
		if href == "" || title == "" {
			continue
		}
		host := hostOf(href)
		if !domainAllowed(host, in.AllowedDomains, in.BlockedDomains) {
			continue
		}
		found++
		b.WriteString(itoa(found) + ". " + title + "\n   " + href + "\n")
		if i < len(snippets) {
			if snip := cleanFragment(snippets[i][1]); snip != "" {
				b.WriteString("   " + snip + "\n")
			}
		}
		b.WriteString("\n")
	}

	if found == 0 {
		// The endpoint answered but the markup did not parse or produced
		// nothing. Reporting this as an empty result set would be a lie about
		// the world rather than about the tool.
		return okResult(
			"websearch: the endpoint responded but no results could be extracted. "+
				"This engine scrapes an HTML endpoint, so a markup change breaks it; "+
				"try WebFetch against a known URL, or a different query.",
			map[string]any{"query": query, "numResults": 0, "parseFailure": true}), nil
	}

	return okResult(b.String(), map[string]any{
		"query": query, "numResults": found,
	}), nil
}

// decodeDDGLink unwraps DuckDuckGo's redirect wrapper.
//
// Its result links are `/l/?uddg=<urlencoded>` rather than the destination, so
// returning them verbatim would hand the model a URL that leads back to the
// search engine.
func decodeDDGLink(href string) string {
	href = strings.TrimSpace(href)
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	if !strings.Contains(href, "uddg=") {
		return href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if target := u.Query().Get("uddg"); target != "" {
		return target
	}
	return href
}

func hostOf(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// domainAllowed applies the include/exclude filters.
//
// A blocked domain wins over an allowed one: the two lists can overlap, and
// the caller writing a block list meant it.
func domainAllowed(host string, allowed, blocked []string) bool {
	for _, d := range blocked {
		if domainMatches(host, d) {
			return false
		}
	}
	if len(allowed) == 0 {
		return true
	}
	for _, d := range allowed {
		if domainMatches(host, d) {
			return true
		}
	}
	return false
}

func domainMatches(host, pattern string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return false
	}
	host = strings.TrimPrefix(host, "www.")
	pattern = strings.TrimPrefix(pattern, "www.")
	return host == pattern || strings.HasSuffix(host, "."+pattern)
}

// cleanFragment strips tags and entities from a scraped HTML fragment.
func cleanFragment(s string) string {
	s = reTag.ReplaceAllString(s, "")
	s = reEntity.Replace(s)
	return strings.TrimSpace(reSpaceRuns.ReplaceAllString(s, " "))
}

func init() {
	tool.RegisterBuiltin(tool.Adapt[WebSearchInput](webSearchTool{spec: spec[WebSearchInput]{
		Name_:            "WebSearch",
		Hint:             "search the web",
		Desc:             webSearchDescription,
		Schema:           webSearchSchema,
		ReadOnly:         true,
		ConcurrencySafe:  true,
		OpenWorld:        true,
		SearchRead:       tool.SearchRead{Search: true},
		NeedsInteraction: false,
	}}))
}
