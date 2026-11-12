package main

// Подписки: https://… ссылка, которая отдаёт список ссылок на серверы (base64 или текстом,
// формат v2rayN/Shadowrocket/«generic»). Панель скачивает её, раскладывает на серверы и держит
// их в servers.json (переживают перезагрузку без интернета). naive из подписки поднимается
// отдельным процессом, как обычный naive. Неизвестные схемы (tt:// и т.п.) пропускаются.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SubInfo — состояние подписки (у Server.Type == "sub").
type SubInfo struct {
	UpdatedAt time.Time      `json:"updated_at,omitempty"` // последнее успешное обновление
	CheckedAt time.Time      `json:"checked_at,omitempty"` // последняя попытка
	Error     string         `json:"error,omitempty"`
	ErrorEN   string         `json:"error_en,omitempty"`
	Skipped   map[string]int `json:"skipped,omitempty"` // схема -> сколько пропущено
	IntervalH int            `json:"interval_h,omitempty"`
	Upload    int64          `json:"upload,omitempty"`
	Download  int64          `json:"download,omitempty"`
	Total     int64          `json:"total,omitempty"`
	Expire    int64          `json:"expire,omitempty"` // unix
	Items     []Server       `json:"items,omitempty"`
}

type subItem struct {
	URI  string
	Name string
	Type string
}

type subResult struct {
	Items     []subItem
	Skipped   map[string]int
	IntervalH int
	Upload    int64
	Download  int64
	Total     int64
	Expire    int64
}

// isSubURL — ссылка на подписку, а не на сервер.
func isSubURL(raw string) bool {
	l := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://")
}

func validateSubURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", E("подписка: нужна ссылка вида https://…", "subscription: need a link like https://...")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return "", E("ссылка не должна содержать пробелов и переносов строк", "link must not contain spaces or line breaks")
	}
	return raw, nil
}

// subHost — адрес подписки для отображения (без токена).
func subHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// FetchSub скачивает и разбирает подписку. proxy — запасной HTTP-прокси (mixed-порт mihomo),
// если напрямую не вышло (в tproxy-режиме трафик самого контейнера мимо mihomo).
func FetchSub(ctx context.Context, raw, proxy, ua string) (*subResult, error) {
	body, hdr, err := subGet(ctx, raw, "", ua)
	if err != nil && proxy != "" {
		var err2 error
		body, hdr, err2 = subGet(ctx, raw, proxy, ua)
		if err2 == nil {
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	res, err := parseSubBody(body)
	if err != nil {
		return nil, err
	}
	res.Upload, res.Download, res.Total, res.Expire = parseUserinfo(hdr.Get("Subscription-Userinfo"))
	if h, err := strconv.Atoi(strings.TrimSpace(hdr.Get("Profile-Update-Interval"))); err == nil && h > 0 {
		res.IntervalH = h
	}
	return res, nil
}

func subGet(ctx context.Context, raw, proxy, ua string) ([]byte, http.Header, error) {
	tr := &http.Transport{Proxy: nil, ResponseHeaderTimeout: 20 * time.Second}
	if proxy != "" {
		pu, err := url.Parse(proxy)
		if err != nil {
			return nil, nil, err
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	hc := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, Ef("подписка недоступна: %v", "subscription unreachable: %v", cleanNetErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, Ef("подписка ответила HTTP %d", "subscription answered HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, Ef("подписка: обрыв при чтении: %v", "subscription: read failed: %v", err)
	}
	return b, resp.Header, nil
}

// cleanNetErr убирает из ошибки URL с токеном подписки.
func cleanNetErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func parseSubBody(b []byte) (*subResult, error) {
	text := strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff"))
	if text == "" {
		return nil, E("подписка пустая", "subscription is empty")
	}
	if !strings.Contains(text, "://") {
		if dec, ok := decodeB64(text); ok {
			text = strings.TrimSpace(dec)
		}
	}
	switch {
	case strings.HasPrefix(text, "{"):
		return nil, E("подписка отдала JSON (sing-box), нужен список ссылок — уберите ?client=… / ?format=… из ссылки",
			"subscription returned JSON (sing-box); a link list is needed - drop ?client=... / ?format=... from the URL")
	case strings.HasPrefix(text, "proxies:") || strings.Contains(text, "\nproxies:"):
		return nil, E("подписка в формате Clash YAML не поддерживается, нужен список ссылок",
			"Clash YAML subscriptions are not supported; a link list is needed")
	}
	res := &subResult{Skipped: map[string]int{}}
	seen := map[string]bool{}
	for _, ln := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, "//") || seen[ln] {
			continue
		}
		seen[ln] = true
		if i := strings.IndexByte(ln, '#'); i >= 0 && strings.ContainsAny(ln[i:], " \t") {
			ln = ln[:i] + strings.NewReplacer(" ", "%20", "\t", "%20").Replace(ln[i:]) // имя с пробелами
		}
		p, err := ParseURI(ln)
		if err != nil {
			scheme, _, ok := strings.Cut(ln, "://")
			if !ok {
				scheme = "?"
			}
			res.Skipped[strings.ToLower(scheme)]++
			continue
		}
		res.Items = append(res.Items, subItem{URI: ln, Name: cleanName(firstNonEmpty(p.Name, hostPort(p))), Type: p.Type})
	}
	if len(res.Items) == 0 {
		return nil, Ef("в подписке нет поддерживаемых ссылок (пропущено: %s)", "no supported links in the subscription (skipped: %s)",
			skippedText(res.Skipped))
	}
	return res, nil
}

func decodeB64(s string) (string, bool) {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), true
		}
	}
	return "", false
}

// parseUserinfo — заголовок Subscription-Userinfo: upload=…; download=…; total=…; expire=…
func parseUserinfo(h string) (up, down, total, expire int64) {
	for _, part := range strings.Split(h, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "upload":
			up = n
		case "download":
			down = n
		case "total":
			total = n
		case "expire":
			expire = n
		}
	}
	return
}

func skippedText(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s://×%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// subDue — пора обновлять подписку. Интервал из заголовка подписки (Profile-Update-Interval)
// или по умолчанию; после ошибки повтор не чаще retry.
func subDue(sv Server, now time.Time, def, retry time.Duration) bool {
	if sv.Type != "sub" || !sv.Enabled {
		return false
	}
	if sv.Sub == nil || sv.Sub.CheckedAt.IsZero() {
		return true
	}
	if sv.Sub.Error != "" {
		return now.Sub(sv.Sub.CheckedAt) >= retry
	}
	iv := def
	if sv.Sub.IntervalH > 0 {
		iv = time.Duration(sv.Sub.IntervalH) * time.Hour
	}
	return now.Sub(sv.Sub.UpdatedAt) >= iv
}
