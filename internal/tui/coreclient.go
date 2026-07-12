package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// coreClient reads the live WSMS memory view from `wsms serve` for the right-
// hand panels. It is read-only: the agent's writes reach the ledger through the
// bridge extension, not through the TUI.
type coreClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newCoreClient(baseURL, token string) *coreClient {
	return &coreClient{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 3 * time.Second},
	}
}

// vizState is the decoded, panel-ready subset of /viz/state. The nested keys are
// PascalCase because serve passes the domain structs (which carry no json tags)
// through verbatim.
type vizState struct {
	reachable bool

	session string
	capsule string

	residentPages int
	maxPages      int
	hotPages      int
	coldPages     int
	pinnedPages   int
	ghostPages    int

	maintCategory string
	maintDegraded bool
	maintPending  int

	embedDegraded bool
	embedCategory string
}

func (c *coreClient) fetchViz(ctx context.Context) (vizState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/viz/state", nil)
	if err != nil {
		return vizState{}, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return vizState{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return vizState{}, fmt.Errorf("viz/state: %s", resp.Status)
	}

	var body struct {
		Session   string `json:"session"`
		Capsule   string `json:"capsule"`
		Residency struct {
			ResidentPages int `json:"ResidentPages"`
			HotPages      int `json:"HotPages"`
			ColdPages     int `json:"ColdPages"`
			PinnedPages   int `json:"PinnedPages"`
			GhostPages    int `json:"GhostPages"`
			Policy        struct {
				MaxResidentPages int `json:"MaxResidentPages"`
			} `json:"Policy"`
		} `json:"residency"`
		Maintenance struct {
			Category string `json:"Category"`
			Degraded bool   `json:"Degraded"`
			Pending  int    `json:"Pending"`
		} `json:"maintenance"`
		Embedding struct {
			Degraded bool   `json:"Degraded"`
			Category string `json:"Category"`
		} `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return vizState{}, err
	}

	return vizState{
		reachable:     true,
		session:       body.Session,
		capsule:       body.Capsule,
		residentPages: body.Residency.ResidentPages,
		maxPages:      body.Residency.Policy.MaxResidentPages,
		hotPages:      body.Residency.HotPages,
		coldPages:     body.Residency.ColdPages,
		pinnedPages:   body.Residency.PinnedPages,
		ghostPages:    body.Residency.GhostPages,
		maintCategory: body.Maintenance.Category,
		maintDegraded: body.Maintenance.Degraded,
		maintPending:  body.Maintenance.Pending,
		embedDegraded: body.Embedding.Degraded,
		embedCategory: body.Embedding.Category,
	}, nil
}
