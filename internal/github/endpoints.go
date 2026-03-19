package github

import (
	"fmt"
	"net/url"
	"strings"
)

type endpointURLs struct {
	apiBase *url.URL
	webBase *url.URL
}

func resolveEndpointURLs(raw string) (endpointURLs, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return endpointURLs{}, fmt.Errorf("parse github.endpoint: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return endpointURLs{}, fmt.Errorf("github.endpoint must include scheme and host")
	}

	switch {
	case strings.EqualFold(parsed.Host, "github.com"), strings.EqualFold(parsed.Host, "api.github.com"):
		return endpointURLs{
			apiBase: &url.URL{Scheme: parsed.Scheme, Host: "api.github.com"},
			webBase: &url.URL{Scheme: parsed.Scheme, Host: "github.com"},
		}, nil
	default:
		webPath := strings.TrimSuffix(parsed.Path, "/")
		apiPath := webPath
		switch {
		case strings.HasSuffix(webPath, "/api/v3"):
			webPath = strings.TrimSuffix(webPath, "/api/v3")
			apiPath = strings.TrimSuffix(webPath, "/") + "/api/v3"
		case strings.HasSuffix(webPath, "/api"):
			webPath = strings.TrimSuffix(webPath, "/api")
			apiPath = strings.TrimSuffix(webPath, "/") + "/api/v3"
		default:
			apiPath = strings.TrimSuffix(webPath, "/") + "/api/v3"
		}

		return endpointURLs{
			apiBase: &url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: strings.TrimSuffix(apiPath, "/")},
			webBase: &url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: strings.TrimSuffix(webPath, "/")},
		}, nil
	}
}

func buildURL(base *url.URL, path string) *url.URL {
	next := *base
	next.RawPath = ""
	next.RawQuery = ""
	next.Fragment = ""
	next.Path = strings.TrimSuffix(base.Path, "/") + path
	return &next
}
