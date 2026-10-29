package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeMihomo struct {
	mu     sync.Mutex
	up, dn int64
	conns  []map[string]any
	groups map[string]map[string]any
}

func (f *fakeMihomo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/connections":
		_ = json.NewEncoder(w).Encode(map[string]any{"uploadTotal": f.up, "downloadTotal": f.dn, "connections": f.conns})
	case strings.HasPrefix(r.URL.Path, "/proxies/"):
		name := strings.TrimPrefix(r.URL.Path, "/proxies/")
		g, ok := f.groups[name]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(g)
	default:
		w.WriteHeader(404)
	}
}

func conn(id, src, host, via string, start time.Time) map[string]any {
	return map[string]any{
		"id": id, "start": start.Format(time.RFC3339Nano), "upload": 10, "download": 20,
		"chains": []string{via, "AUTO", "SELECTOR"}, "rule": "InName", "rulePayload": "tun-in",
		"metadata": map[string]any{"network": "tcp", "sourceIP": src, "host": host, "destinationIP": "1.2.3.4",
			"destinationPort": "443", "inboundName": "tun-in"},
	}
}

func TestMonitorPoll(t *testing.T) {
	now := time.Now()
	f := &fakeMihomo{
		conns: []map[string]any{conn("a", "192.168.1.10", "rr1.googlevideo.com", "DE", now)},
		groups: map[string]map[string]any{
			"SELECTOR": {"name": "SELECTOR", "now": "AUTO"},
			"AUTO":     {"name": "AUTO", "now": "DE"},
			"DE":       {"name": "DE", "alive": true, "history": []map[string]int{{"delay": 85}}},
		},
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewMonitor(NewMihomo(srv.URL, "", "WEB", ""), lg, time.Second, time.Minute, false, "SELECTOR", "off")
	ctx := context.Background()

	m.poll(ctx)
	m.pollVia(ctx)
	a := m.Activity(10)
	if !a.OK || a.Conns != 1 || len(a.Recent) != 1 || a.Recent[0].Via != "DE" || a.Recent[0].Host != "rr1.googlevideo.com" {
		t.Fatalf("first poll: %+v", a)
	}
	if a.Via != "DE" || !a.ViaAlive || a.ViaDelay != 85 || strings.Join(a.ViaPath, ">") != "SELECTOR>AUTO>DE" {
		t.Fatalf("via: %q %v %d %v", a.Via, a.ViaAlive, a.ViaDelay, a.ViaPath)
	}

	// второе соединение + трафик; первое уже видели — в recent только новое
	f.mu.Lock()
	f.up, f.dn = 1000, 8000
	f.conns = append(f.conns, conn("b", "192.168.1.11", "", "DIRECT", now.Add(time.Second)))
	f.mu.Unlock()
	m.mu.Lock()
	m.lastPoll = time.Now().Add(-2 * time.Second)
	m.mu.Unlock()
	m.poll(ctx)
	a = m.Activity(10)
	if len(a.Recent) != 2 || a.Recent[0].ID != "b" || !a.Recent[0].Direct || a.Recent[0].Host != "" {
		t.Fatalf("recent: %+v", a.Recent)
	}
	if a.DownRate < 3000 || a.DownRate > 4500 || a.ByVia["DIRECT"] != 1 || a.ByVia["DE"] != 1 {
		t.Fatalf("rate/byVia: %d %v", a.DownRate, a.ByVia)
	}
	if m.perHosts["googlevideo.com"] != 1 || m.perHosts["1.2.3.4"] != 1 {
		t.Fatalf("hosts: %v", m.perHosts)
	}

	// mihomo перезапустился: счётчики меньше прежних — скорость 0, не отрицательная
	f.mu.Lock()
	f.up, f.dn = 10, 10
	f.mu.Unlock()
	m.poll(ctx)
	if a = m.Activity(1); a.DownRate != 0 || a.UpRate != 0 {
		t.Fatalf("reset rate: %d %d", a.DownRate, a.UpRate)
	}

	// сервер пропал → событие
	f.mu.Lock()
	f.groups["DE"]["alive"] = false
	f.mu.Unlock()
	m.pollVia(ctx)
	if a = m.Activity(1); len(a.Events) < 2 || !strings.Contains(a.Events[0].Msg, "нет связи") {
		t.Fatalf("events: %+v", a.Events)
	}
}

func TestMonitorViaUnlistedLeaf(t *testing.T) {
	f := &fakeMihomo{groups: map[string]map[string]any{
		"SELECTOR": {"name": "SELECTOR", "now": "AUTO"},
		"AUTO":     {"name": "AUTO", "now": "Нет серверов (напрямую)", "alive": true},
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	m := NewMonitor(NewMihomo(srv.URL, "", "WEB", ""), slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, 0, false, "SELECTOR", "off")
	m.pollVia(context.Background())
	if a := m.Activity(1); a.Via != "Нет серверов (напрямую)" || !a.ViaAlive || a.ViaDelay != -1 || len(a.ViaPath) != 3 {
		t.Fatalf("via: %q %v %d %v", a.Via, a.ViaAlive, a.ViaDelay, a.ViaPath)
	}
}

func TestMonitorLogsFilter(t *testing.T) {
	m := &Monitor{}
	for i, s := range []string{"[TCP] a --> youtube.com:443", "[UDP] b --> x.com:443", "[TCP] c --> YouTube.com:80"} {
		m.logRing = append(m.logRing, LogLine{Seq: int64(i + 1), Text: s})
	}
	if got := m.Logs(0, 10, "youtube"); len(got) != 2 {
		t.Fatalf("filter: %+v", got)
	}
	if got := m.Logs(2, 10, ""); len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("after: %+v", got)
	}
	if got := m.Logs(0, 1, ""); len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("limit keeps newest: %+v", got)
	}
}

func TestPadRunes(t *testing.T) {
	if got := pad("Нидерланды", 5); got != "Ниде…" {
		t.Fatalf("cut: %q", got)
	}
	if got := pad("DE", 4); got != "DE  " {
		t.Fatalf("pad: %q", got)
	}
	if got := padL("85 мс", 7); got != "  85 мс" {
		t.Fatalf("padL: %q", got)
	}
	if m, ok := modeArg("авто"); !ok || m != "AUTO" {
		t.Fatal("modeArg")
	}
}
