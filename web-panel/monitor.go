package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Monitor — живая картина трафика: скорость, соединения, новые запросы, журнал mihomo.
// Пишет в stdout (журнал RouterOS) смену сервера, пропажу/возврат связи и периодическую сводку.
type Monitor struct {
	mh  *Mihomo
	log *slog.Logger

	interval    time.Duration // опрос /connections
	statusEvery time.Duration // сводка в журнал; 0 = выкл
	logConns    bool          // каждое новое соединение в журнал
	selGroup    string
	streamLevel string // уровень потока /logs для веба

	mu       sync.Mutex
	ok       bool
	err      string
	lastPoll time.Time
	upTotal  int64
	dnTotal  int64
	upRate   int64 // байт/с
	dnRate   int64
	conns    []ConnView
	hist     []RatePoint
	recent   []ConnView // новые соединения, свежие в конце
	seen     map[string]bool
	events   []Event
	via      string // фактический сервер (конец цепочки групп)
	viaPath  []string
	viaAlive bool
	viaDelay int

	// за период сводки
	perNew   int
	perHosts map[string]int
	perUp    int64
	perDn    int64

	logMu   sync.Mutex
	logSeq  int64
	logRing []LogLine
}

type ConnView struct {
	ID       string    `json:"id"`
	Start    time.Time `json:"start"`
	Net      string    `json:"net"`
	Src      string    `json:"src"`
	Host     string    `json:"host"`
	DstIP    string    `json:"dst_ip"`
	Port     string    `json:"port"`
	Via      string    `json:"via"`
	Chain    []string  `json:"chain"`
	Rule     string    `json:"rule"`
	Up       int64     `json:"up"`
	Down     int64     `json:"down"`
	Inbound  string    `json:"inbound"`
	Direct   bool      `json:"direct"` // клиентский трафик напрямую — признак утечки
	Rejected bool      `json:"rejected"`
	Internal bool      `json:"internal"`
}

type RatePoint struct {
	T    int64 `json:"t"`
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
	N    int   `json:"n"`
}

type Event struct {
	T     time.Time `json:"t"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
}

type LogLine struct {
	Seq   int64     `json:"seq"`
	T     time.Time `json:"t"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
}

const (
	histMax   = 180 // 180 точек × 5 с = 15 минут
	recentMax = 200
	eventsMax = 100
	logMax    = 500
)

func NewMonitor(mh *Mihomo, l *slog.Logger, interval, statusEvery time.Duration, logConns bool, selGroup, streamLevel string) *Monitor {
	return &Monitor{mh: mh, log: l, interval: interval, statusEvery: statusEvery, logConns: logConns,
		selGroup: selGroup, streamLevel: streamLevel, seen: map[string]bool{}, perHosts: map[string]int{}, viaDelay: -1}
}

func (m *Monitor) Run(ctx context.Context) {
	if m.interval <= 0 {
		return
	}
	go m.streamLogs(ctx)
	tick := time.NewTicker(m.interval)
	defer tick.Stop()
	viaTick := time.NewTicker(15 * time.Second)
	defer viaTick.Stop()
	var statusC <-chan time.Time
	if m.statusEvery > 0 {
		st := time.NewTicker(m.statusEvery)
		defer st.Stop()
		statusC = st.C
	}
	m.poll(ctx)
	m.pollVia(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.poll(ctx)
			m.mu.Lock()
			unknown := m.via == ""
			m.mu.Unlock()
			if unknown { // mihomo поднялся позже панели — не ждём 15 с
				m.pollVia(ctx)
			}
		case <-viaTick.C:
			m.pollVia(ctx)
		case <-statusC:
			m.logStatus()
		}
	}
}

type rawConn struct {
	ID       string `json:"id"`
	Metadata struct {
		Network         string `json:"network"`
		Type            string `json:"type"`
		SourceIP        string `json:"sourceIP"`
		DestinationIP   string `json:"destinationIP"`
		DestinationPort string `json:"destinationPort"`
		Host            string `json:"host"`
		SniffHost       string `json:"sniffHost"`
		InboundName     string `json:"inboundName"`
	} `json:"metadata"`
	Upload      int64     `json:"upload"`
	Download    int64     `json:"download"`
	Start       time.Time `json:"start"`
	Chains      []string  `json:"chains"`
	Rule        string    `json:"rule"`
	RulePayload string    `json:"rulePayload"`
}

func (m *Monitor) poll(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var raw struct {
		DownloadTotal int64     `json:"downloadTotal"`
		UploadTotal   int64     `json:"uploadTotal"`
		Connections   []rawConn `json:"connections"`
	}
	err := m.mh.do(cctx, http.MethodGet, "/connections", nil, &raw)
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		if m.ok {
			m.eventLocked("warn", "mihomo API недоступен: "+err.Error())
			m.log.Warn("mihomo api unreachable", "err", err)
		}
		m.ok, m.err = false, err.Error()
		return
	}
	if !m.ok && !m.lastPoll.IsZero() {
		m.eventLocked("info", "mihomo API снова отвечает")
	}
	m.ok, m.err = true, ""

	if !m.lastPoll.IsZero() {
		dt := now.Sub(m.lastPoll).Seconds()
		du, dd := raw.UploadTotal-m.upTotal, raw.DownloadTotal-m.dnTotal
		if du < 0 || dd < 0 { // mihomo перезапустился — счётчики обнулились
			du, dd = 0, 0
		}
		if dt > 0 {
			m.upRate, m.dnRate = int64(float64(du)/dt), int64(float64(dd)/dt)
		}
		m.perUp += du
		m.perDn += dd
	}
	m.upTotal, m.dnTotal, m.lastPoll = raw.UploadTotal, raw.DownloadTotal, now

	views := make([]ConnView, 0, len(raw.Connections))
	alive := make(map[string]bool, len(raw.Connections))
	var fresh []ConnView
	for _, c := range raw.Connections {
		v := toView(c)
		views = append(views, v)
		alive[c.ID] = true
		if !m.seen[c.ID] {
			fresh = append(fresh, v)
		}
	}
	m.seen = alive
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Start.Before(fresh[j].Start) })
	for _, v := range fresh {
		m.recent = append(m.recent, v)
		m.perNew++
		m.perHosts[hostKey(v)]++
		if m.logConns {
			m.log.Info(fmt.Sprintf("conn %s %s -> %s:%s via %s rule %s", v.Net, v.Src, firstNonEmpty(v.Host, v.DstIP), v.Port, v.Via, v.Rule))
		}
	}
	if n := len(m.recent); n > recentMax {
		m.recent = append([]ConnView(nil), m.recent[n-recentMax:]...)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Start.After(views[j].Start) })
	m.conns = views
	m.hist = append(m.hist, RatePoint{T: now.Unix(), Up: m.upRate, Down: m.dnRate, N: len(views)})
	if n := len(m.hist); n > histMax {
		m.hist = append([]RatePoint(nil), m.hist[n-histMax:]...)
	}
}

func toView(c rawConn) ConnView {
	v := ConnView{
		ID: c.ID, Start: c.Start, Net: strings.ToUpper(c.Metadata.Network), Src: c.Metadata.SourceIP,
		Host: firstNonEmpty(c.Metadata.Host, c.Metadata.SniffHost), DstIP: c.Metadata.DestinationIP,
		Port: c.Metadata.DestinationPort, Chain: c.Chains, Up: c.Upload, Down: c.Download,
		Inbound: firstNonEmpty(c.Metadata.InboundName, c.Metadata.Type),
	}
	if len(c.Chains) > 0 {
		v.Via = c.Chains[0] // реальный выход; последний элемент — первая сработавшая группа
	}
	v.Rule = c.Rule
	if c.RulePayload != "" {
		v.Rule += "(" + c.RulePayload + ")"
	}
	v.Internal = v.Inbound == "Inner" // собственные запросы mihomo: скачивание UI, провайдеров, проверки
	v.Direct = v.Via == "DIRECT" && !v.Internal
	v.Rejected = strings.HasPrefix(v.Via, "REJECT")
	return v
}

func hostKey(v ConnView) string {
	h := firstNonEmpty(v.Host, v.DstIP)
	// youtube.com вместо rr3---sn-xyz.googlevideo.com: два последних уровня
	if !isIP(h) {
		parts := strings.Split(h, ".")
		if len(parts) > 2 {
			h = strings.Join(parts[len(parts)-2:], ".")
		}
	}
	return h
}

// pollVia — какой сервер реально выбран (SELECTOR → AUTO → DE) и жив ли он.
func (m *Monitor) pollVia(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	path := []string{}
	name := m.selGroup
	var leaf ProxyInfo
	for i := 0; i < 6 && name != ""; i++ {
		p, err := m.mh.Group(cctx, name)
		if err != nil {
			if i == 0 {
				return // mihomo недоступен
			}
			// прокси из compatible-провайдера (заглушка «Нет серверов») в /proxies не виден:
			// берём имя из now родителя и его alive, задержка неизвестна
			path = append(path, name)
			leaf = ProxyInfo{Name: name, Alive: leaf.Alive}
			break
		}
		path = append(path, name)
		leaf = p
		if p.Now == "" {
			break
		}
		name = p.Now
	}
	via := path[len(path)-1]
	alive, delay := leaf.Alive, leaf.LastDelay()

	m.mu.Lock()
	defer m.mu.Unlock()
	prev, prevAlive := m.via, m.viaAlive
	m.via, m.viaPath, m.viaAlive, m.viaDelay = via, path, alive, delay
	switch {
	case prev == "":
		m.eventLocked("info", fmt.Sprintf("трафик идёт через %s (%s)", via, strings.Join(path, " → ")))
		m.log.Info(fmt.Sprintf("route: via %s (%s) alive=%s delay=%s", via, strings.Join(path, ">"), yesno(alive), msStr(delay)))
	case prev != via:
		m.eventLocked("warn", fmt.Sprintf("сервер сменился: %s → %s", prev, via))
		m.log.Warn(fmt.Sprintf("route changed: %s -> %s (%s)", prev, via, strings.Join(path, ">")))
	case prevAlive && !alive:
		m.eventLocked("bad", fmt.Sprintf("нет связи с %s", via))
		m.log.Warn(fmt.Sprintf("server %s DOWN", via))
	case !prevAlive && alive:
		m.eventLocked("info", fmt.Sprintf("связь с %s восстановлена, %s", via, msStr(delay)))
		m.log.Info(fmt.Sprintf("server %s UP delay=%s", via, msStr(delay)))
	}
}

func (m *Monitor) logStatus() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ok {
		m.log.Warn("status: mihomo api unreachable", "err", m.err)
		return
	}
	type kv struct {
		k string
		n int
	}
	var top []kv
	for k, n := range m.perHosts {
		top = append(top, kv{k, n})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n || (top[i].n == top[j].n && top[i].k < top[j].k) })
	var tops []string
	for i := 0; i < len(top) && i < 5; i++ {
		tops = append(tops, fmt.Sprintf("%s:%d", top[i].k, top[i].n))
	}
	per := m.statusEvery.Round(time.Second).String()
	m.log.Info(fmt.Sprintf("status: via=%s alive=%s delay=%s conns=%d rate down=%s up=%s | last %s: new=%d down=%s up=%s top=%s",
		firstNonEmpty(m.via, "?"), yesno(m.viaAlive), msStr(m.viaDelay), len(m.conns),
		rateStr(m.dnRate), rateStr(m.upRate), per, m.perNew, bytesStr(m.perDn), bytesStr(m.perUp), firstNonEmpty(strings.Join(tops, ","), "-")))
	m.perNew, m.perUp, m.perDn = 0, 0, 0
	m.perHosts = map[string]int{}
}

func (m *Monitor) eventLocked(level, msg string) {
	m.events = append(m.events, Event{T: time.Now(), Level: level, Msg: msg})
	if n := len(m.events); n > eventsMax {
		m.events = append([]Event(nil), m.events[n-eventsMax:]...)
	}
}

// Event — событие панели (смена сервера и т.п.) для веба/консоли.
func (m *Monitor) Event(level, msg string) {
	m.mu.Lock()
	m.eventLocked(level, msg)
	m.mu.Unlock()
}

type Activity struct {
	OK       bool           `json:"ok"`
	Error    string         `json:"error,omitempty"`
	Interval int            `json:"interval"`
	Via      string         `json:"via"`
	ViaPath  []string       `json:"via_path"`
	ViaAlive bool           `json:"via_alive"`
	ViaDelay int            `json:"via_delay"`
	UpRate   int64          `json:"up_rate"`
	DownRate int64          `json:"down_rate"`
	UpTotal  int64          `json:"up_total"`
	DnTotal  int64          `json:"down_total"`
	Conns    int            `json:"conns"`
	ByVia    map[string]int `json:"by_via"`
	History  []RatePoint    `json:"history"`
	Recent   []ConnView     `json:"recent"`
	Events   []Event        `json:"events"`
}

func (m *Monitor) Activity(recentN int) Activity {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := Activity{OK: m.ok, Error: m.err, Interval: int(m.interval / time.Second), Via: m.via, ViaPath: m.viaPath,
		ViaAlive: m.viaAlive, ViaDelay: m.viaDelay, UpRate: m.upRate, DownRate: m.dnRate,
		UpTotal: m.upTotal, DnTotal: m.dnTotal, Conns: len(m.conns), ByVia: map[string]int{}}
	for _, c := range m.conns {
		if c.Internal {
			a.ByVia["служебные"]++
			continue
		}
		a.ByVia[c.Via]++
	}
	a.History = append([]RatePoint(nil), m.hist...)
	if recentN <= 0 || recentN > len(m.recent) {
		recentN = len(m.recent)
	}
	for i := len(m.recent) - 1; i >= len(m.recent)-recentN; i-- { // свежие первыми
		a.Recent = append(a.Recent, m.recent[i])
	}
	for i := len(m.events) - 1; i >= 0; i-- {
		a.Events = append(a.Events, m.events[i])
	}
	return a
}

func (m *Monitor) Connections() []ConnView {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ConnView(nil), m.conns...)
}

// streamLogs держит подписку на /logs mihomo. Поток не зависит от log-level конфига:
// в журнал RouterOS идёт warning, а сюда — info (каждый запрос).
func (m *Monitor) streamLogs(ctx context.Context) {
	if m.streamLevel == "" || m.streamLevel == "off" {
		return
	}
	hc := &http.Client{} // без таймаута: поток бесконечный
	for ctx.Err() == nil {
		err := m.readLogStream(ctx, hc)
		if ctx.Err() != nil {
			return
		}
		m.log.Debug("log stream ended", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (m *Monitor) readLogStream(ctx context.Context, hc *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.mh.base+"/logs?level="+url.QueryEscape(m.streamLevel), nil)
	if err != nil {
		return err
	}
	if m.mh.secret != "" {
		req.Header.Set("Authorization", "Bearer "+m.mh.secret)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e struct {
			Type    string `json:"type"`
			Payload string `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Payload == "" {
			continue
		}
		m.logMu.Lock()
		m.logSeq++
		m.logRing = append(m.logRing, LogLine{Seq: m.logSeq, T: time.Now(), Level: e.Type, Text: e.Payload})
		if n := len(m.logRing); n > logMax {
			m.logRing = append([]LogLine(nil), m.logRing[n-logMax:]...)
		}
		m.logMu.Unlock()
	}
	return sc.Err()
}

// Logs — строки журнала mihomo после seq (0 = последние limit строк).
func (m *Monitor) Logs(after int64, limit int, filter string) []LogLine {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	filter = strings.ToLower(filter)
	out := []LogLine{}
	for _, l := range m.logRing {
		if l.Seq <= after {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(l.Text), filter) {
			continue
		}
		out = append(out, l)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func msStr(d int) string {
	switch {
	case d < 0:
		return "?"
	case d == 0:
		return "timeout"
	}
	return strconv.Itoa(d) + "ms"
}

func rateStr(bps int64) string {
	bits := float64(bps) * 8
	switch {
	case bits >= 1e6:
		return fmt.Sprintf("%.1fMbit/s", bits/1e6)
	case bits >= 1e3:
		return fmt.Sprintf("%.0fkbit/s", bits/1e3)
	}
	return fmt.Sprintf("%.0fbit/s", bits)
}

func bytesStr(b int64) string {
	f := float64(b)
	switch {
	case f >= 1<<30:
		return fmt.Sprintf("%.2fGB", f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%.1fMB", f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%.0fKB", f/(1<<10))
	}
	return fmt.Sprintf("%dB", b)
}
