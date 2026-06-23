package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HA is a thin Home Assistant REST client.
type HA struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func NewHA(baseURL, token string) *HA {
	return &HA{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		Client:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (h *HA) do(method, path string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequest(method, h.BaseURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

// Ping returns the status of GET /api/ (200 = reachable + token accepted).
func (h *HA) Ping() (int, error) {
	st, _, err := h.do(http.MethodGet, "/api/", nil)
	return st, err
}

// Entity is a subset of an entity's state object.
type Entity struct {
	EntityID   string         `json:"entity_id"`
	State      string         `json:"state"`
	Attributes map[string]any `json:"attributes"`
}

func (e Entity) FriendlyName() string {
	if n, ok := e.Attributes["friendly_name"].(string); ok {
		return n
	}
	return e.EntityID
}

func (h *HA) States() ([]Entity, error) {
	st, b, err := h.do(http.MethodGet, "/api/states", nil)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("GET /api/states: HTTP %d", st)
	}
	var es []Entity
	if err := json.Unmarshal(b, &es); err != nil {
		return nil, err
	}
	return es, nil
}

// Services maps domain -> set of service names registered in HA.
type Services map[string]map[string]bool

func (s Services) Has(domain, service string) bool {
	return s[domain][service]
}

func (h *HA) Services() (Services, error) {
	st, b, err := h.do(http.MethodGet, "/api/services", nil)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("GET /api/services: HTTP %d", st)
	}
	var raw []struct {
		Domain   string                     `json:"domain"`
		Services map[string]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	out := Services{}
	for _, d := range raw {
		set := map[string]bool{}
		for name := range d.Services {
			set[name] = true
		}
		out[d.Domain] = set
	}
	return out, nil
}

// CallService POSTs a service call. Success is HTTP 200/201.
func (h *HA) CallService(c ServiceCall) error {
	body, _ := json.Marshal(c.Data)
	st, resp, err := h.do(http.MethodPost, "/api/services/"+c.Domain+"/"+c.Service, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if st != http.StatusOK && st != http.StatusCreated {
		return fmt.Errorf("HTTP %d: %s", st, strings.TrimSpace(string(resp)))
	}
	return nil
}
