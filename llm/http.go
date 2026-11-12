package llm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// httpClient is the shared client for catalog fetches.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// jsonUnmarshal is a thin wrapper so callers do not import encoding/json.
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// fetchJSON GETs a URL with an optional bearer token and returns the body.
func fetchJSON(endpoint, apiKey string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: %s", resp.Status, body)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}
