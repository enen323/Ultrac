package builtin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"ultrac/internal/contract/tool"
)

// ---------------------------------------------------------------------------
// WebFetch
// ---------------------------------------------------------------------------

// WebFetchInput mirrors WebFetchTool's schema.
type WebFetchInput struct {
	URL    string `json:"url"`
	Prompt string `json:"prompt"`
}

const webFetchSchema = `{
  "type": "object",
  "properties": {
    "url": {
      "type": "string",
      "description": "The URL to fetch content from"
    },
    "prompt": {
      "type": "string",
      "description": "The prompt to run on the fetched content"
    }
  },
  "required": ["url", "prompt"],
  "additionalProperties": false
}`

const webFetchDescription = `- Fetches content from a specified URL and returns it as markdown text
- Takes a URL and a prompt as input
- Fetches the URL content, converts HTML to markdown, and returns the content alongside the prompt so the caller can summarise it
- This engine does not call a model to summarise; it returns the converted page. Treat the prompt as the question you are about to answer from the content.
- Use this tool when you need to retrieve and analyze web content
- IMPORTANT: If an MCP-provided web fetch tool is available, prefer using that tool instead of this one, as it may have fewer restrictions
- The URL must be a fully-formed valid URL
- HTTP URLs will be automatically upgraded to HTTPS`

// fetchTimeout bounds one page fetch. Long enough for a slow origin, short
// enough that a hanging host does not consume the whole turn.
const fetchTimeout = 30 * time.Second

// maxFetchBytes bounds what is read from a page. A response with no
// Content-Length can stream forever, and the limit has to apply during the
// read, not after it.
const maxFetchBytes = 5 * 1024 * 1024

type webFetchTool struct {
	spec[WebFetchInput]
}

func (webFetchTool) Call(ctx context.Context, in WebFetchInput, _ *tool.UseContext, _ tool.CanUseFunc, _ tool.ProgressFunc) (*tool.Result, error) {
	raw := strings.TrimSpace(in.URL)
	if raw == "" {
		return errResult("webfetch: url is required"), nil
	}
	// Upgrade bare hosts to https, matching the tool's documented behaviour: a
	// user who typed example.com meant the site, not a scheme-less string.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return errResult("webfetch: %q is not a valid URL: %v", in.URL, err), nil
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "http" {
		parsed.Scheme = "https"
		scheme = "https"
	}
	if scheme != "https" {
		return errResult("webfetch: only https URLs are supported; got %q", parsed.Scheme), nil
	}
	if parsed.Host == "" {
		return errResult("webfetch: %q has no host", in.URL), nil
	}

	body, contentType, err := doFetch(ctx, parsed.String(), "text/html, text/plain, application/json;q=0.9, */*;q=0.1")
	if err != nil {
		return errResult("webfetch: %v", err), nil
	}

	text := string(body)
	if strings.Contains(contentType, "html") {
		text = htmlToText(text)
	}

	var b strings.Builder
	b.WriteString("Fetched " + parsed.String() + "\n")
	if contentType != "" {
		b.WriteString("Content-Type: " + contentType + "\n")
	}
	b.WriteString("\n")
	if strings.TrimSpace(in.Prompt) != "" {
		b.WriteString("Prompt: " + in.Prompt + "\n\n")
	}
	b.WriteString(text)

	return okResult(b.String(), map[string]any{
		"url": parsed.String(), "contentType": contentType,
		"bytes": len(body), "textBytes": len(text),
	}), nil
}

// doFetch performs one GET with the shared limits.
func doFetch(ctx context.Context, target, accept string) ([]byte, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Accept", accept)

	client := &http.Client{
		// Redirects are followed but capped, so a redirect loop cannot spin.
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			if strings.ToLower(r.URL.Scheme) != "https" {
				return fmt.Errorf("refusing to follow a redirect to %s", r.URL.Scheme)
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	limited := io.LimitReader(resp.Body, maxFetchBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > maxFetchBytes {
		return nil, "", fmt.Errorf("response is larger than %s; refusing to load it", humanBytes(maxFetchBytes))
	}
	return body, resp.Header.Get("Content-Type"), nil
}

func userAgent() string {
	return "ultrac/1.0 (+https://github.com/; Go engine)"
}

// ---------------------------------------------------------------------------
// HTML to text
// ---------------------------------------------------------------------------

var (
	// Go's regexp is RE2: no backreferences, so the tags are spelled out
	// rather than captured and reused. Each alternative is non-greedy, which
	// matters because pages routinely nest two of these.
	reScriptStyle = regexp.MustCompile(`(?is)<script[^>]*>.*?</script\s*>` +
		`|<style[^>]*>.*?</style\s*>` +
		`|<noscript[^>]*>.*?</noscript\s*>` +
		`|<svg[^>]*>.*?</svg\s*>` +
		`|<head[^>]*>.*?</head\s*>` +
		`|<!--.*?-->`)
	reBlockClose = regexp.MustCompile(`(?i)</\s*(p|div|section|article|li|tr|h[1-6]|blockquote|pre|table|ul|ol)\s*>`)
	reBr         = regexp.MustCompile(`(?i)<br\s*/?>`)
	reLink       = regexp.MustCompile(`(?is)<a\b[^>]*href\s*=\s*["']?([^"'\s>]+)["']?[^>]*>(.*?)</a\s*>`)
	reTag        = regexp.MustCompile(`(?s)<[^>]*>`)
	reEntity     = strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&apos;", "'", "&mdash;", "—",
		"&ndash;", "–", "&hellip;", "…", "&rsquo;", "'", "&lsquo;", "'",
		"&ldquo;", `"`, "&rdquo;", `"`,
	)
	reBlankRuns = regexp.MustCompile(`\n{3,}`)
	reSpaceRuns = regexp.MustCompile(`[ \t]{2,}`)
)

// htmlToText converts a page to readable text.
//
// A regexp stripper, not an HTML parser, and deliberately so: it is used to
// give the model something it can read, and being wrong about a tag boundary
// costs a stray character. Pulling in a full parser would add a dependency for
// output that a model reads as prose.
func htmlToText(html string) string {
	s := reScriptStyle.ReplaceAllString(html, " ")
	// Keep link targets, because a page's value is often its links.
	s = reLink.ReplaceAllString(s, "$2 ($1)")
	s = reBr.ReplaceAllString(s, "\n")
	s = reBlockClose.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = reEntity.Replace(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, l := range lines {
		l = strings.TrimSpace(l)
		l = reSpaceRuns.ReplaceAllString(l, " ")
		if l == "" {
			continue
		}
		kept = append(kept, l)
	}
	out := strings.Join(kept, "\n")
	return reBlankRuns.ReplaceAllString(out, "\n\n")
}

func init() {
	tool.RegisterBuiltin(tool.Adapt[WebFetchInput](webFetchTool{spec: spec[WebFetchInput]{
		Name_:           "WebFetch",
		Hint:            "fetch a URL and return its text",
		Desc:            webFetchDescription,
		Schema:          webFetchSchema,
		ReadOnly:        true,
		ConcurrencySafe: true,
		OpenWorld:       true,
		SearchRead:      tool.SearchRead{Read: true},
	}}))
}
