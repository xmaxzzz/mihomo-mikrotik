package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Mihomo — клиент external-controller API.
type Mihomo struct {
	base     string
	secret   string
	provider string
	testURL  string
	hc       *http.Client
}

func NewMihomo(base, secret, provider, testURL string) *Mihomo {
	return &Mihomo{base: base, secret: secret, provider: provider, testURL: testURL,
		hc: &http.Client{Timeout: 35 * time.Second}}
}

func (m *Mihomo) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, rd)
	if err != nil {
		return err
	}
	if m.secret != "" {
		req.Header.Set("Authorization", "Bearer "+m.secret)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		return fmt.Errorf("mihomo недоступен: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = string(data)
		}
		return fmt.Errorf("mihomo %s %s: %d %s", method, path, resp.StatusCode, e.Message)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type ProxyInfo struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Alive   bool   `json:"alive"`
	Now     string `json:"now"`
	History []struct {
		Delay int `json:"delay"`
	} `json:"history"`
}

func (p ProxyInfo) LastDelay() int {
	if len(p.History) == 0 {
		return -1 // ещё не проверялся
	}
	return p.History[len(p.History)-1].Delay
}

func (m *Mihomo) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	err := m.do(ctx, http.MethodGet, "/version", nil, &v)
	return v.Version, err
}

// ReloadProvider перечитывает servers.txt без перезапуска mihomo.
func (m *Mihomo) ReloadProvider(ctx context.Context) error {
	return m.do(ctx, http.MethodPut, "/providers/proxies/"+url.PathEscape(m.provider), nil, nil)
}

func (m *Mihomo) ProviderProxies(ctx context.Context) (map[string]ProxyInfo, error) {
	var p struct {
		Proxies []ProxyInfo `json:"proxies"`
	}
	if err := m.do(ctx, http.MethodGet, "/providers/proxies/"+url.PathEscape(m.provider), nil, &p); err != nil {
		return nil, err
	}
	out := make(map[string]ProxyInfo, len(p.Proxies))
	for _, x := range p.Proxies {
		out[x.Name] = x
	}
	return out, nil
}

func (m *Mihomo) HealthCheckAll(ctx context.Context) error {
	return m.do(ctx, http.MethodGet, "/providers/proxies/"+url.PathEscape(m.provider)+"/healthcheck", nil, nil)
}

// Delay — проверка одного прокси, мс. 0 = таймаут/ошибка.
func (m *Mihomo) Delay(ctx context.Context, name string, timeoutMs int) (int, error) {
	q := url.Values{}
	q.Set("url", m.testURL)
	q.Set("timeout", strconv.Itoa(timeoutMs))
	var r struct {
		Delay int `json:"delay"`
	}
	path := "/providers/proxies/" + url.PathEscape(m.provider) + "/" + url.PathEscape(name) + "/healthcheck?" + q.Encode()
	err := m.do(ctx, http.MethodGet, path, nil, &r)
	return r.Delay, err
}

func (m *Mihomo) Group(ctx context.Context, name string) (ProxyInfo, error) {
	var p ProxyInfo
	err := m.do(ctx, http.MethodGet, "/proxies/"+url.PathEscape(name), nil, &p)
	return p, err
}

func (m *Mihomo) Select(ctx context.Context, group, name string) error {
	return m.do(ctx, http.MethodPut, "/proxies/"+url.PathEscape(group), map[string]string{"name": name}, nil)
}

// CloseConnections рвёт текущие соединения, чтобы смена сервера применилась сразу.
func (m *Mihomo) CloseConnections(ctx context.Context) error {
	return m.do(ctx, http.MethodDelete, "/connections", nil, nil)
}
