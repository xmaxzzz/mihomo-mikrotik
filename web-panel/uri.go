package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Поддерживаемые схемы ссылок -> тип сервера в панели.
// Всё, кроме naive, парсит сам mihomo (common/convert), naive поднимается
// отдельным процессом и отдаётся в mihomo как локальный socks5.
var schemeTypes = map[string]string{
	"vless":       "vless",
	"hysteria2":   "hysteria2",
	"hy2":         "hysteria2",
	"mierus":      "mieru",
	"naive+https": "naive",
	"naive+quic":  "naive",
	"vmess":       "vmess",
	"trojan":      "trojan",
	"ss":          "ss",
	"tuic":        "tuic",
	"anytls":      "anytls",
	"hysteria":    "hysteria",
	"socks":       "socks5",
	"socks5":      "socks5",
}

// ParsedURI — то, что панель понимает о ссылке (для отображения и валидации).
type ParsedURI struct {
	Scheme string
	Type   string
	Host   string
	Port   string
	Name   string // из #fragment (или ps у vmess)
}

var errEmpty = E("пустая ссылка", "empty link")

func ParseURI(raw string) (*ParsedURI, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errEmpty
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return nil, E("ссылка не должна содержать пробелов и переносов строк", "link must not contain spaces or line breaks")
	}
	scheme, body, ok := strings.Cut(raw, "://")
	if !ok || body == "" {
		return nil, E("не похоже на ссылку: нет схемы вида vless://", "not a link: no scheme like vless://")
	}
	scheme = strings.ToLower(scheme)
	typ, ok := schemeTypes[scheme]
	if !ok {
		return nil, Ef("схема %q не поддерживается", "scheme %q is not supported", scheme)
	}
	p := &ParsedURI{Scheme: scheme, Type: typ}

	if typ == "vmess" {
		m, err := decodeVmess(body)
		if err != nil {
			// vmess в формате url (не base64) — разбираем как обычную ссылку
			return parseGeneric(p, body)
		}
		p.Host = fmt.Sprint(m["add"])
		p.Port = fmt.Sprint(m["port"])
		if ps, ok := m["ps"].(string); ok {
			p.Name = ps
		}
		if p.Host == "" || p.Host == "<nil>" {
			return nil, E("vmess: нет адреса сервера", "vmess: no server address")
		}
		return p, nil
	}

	p2, err := parseGeneric(p, body)
	if err != nil {
		return nil, err
	}

	switch typ {
	case "mieru":
		q := queryOf(raw)
		ports, protos := q["port"], q["protocol"]
		if len(ports) == 0 || len(ports) != len(protos) {
			return nil, E("mieru: нужно одинаковое количество параметров port и protocol", "mieru: port and protocol must come in equal numbers")
		}
		for _, pr := range protos {
			if pr != "TCP" && pr != "UDP" {
				return nil, Ef("mieru: protocol должен быть TCP или UDP, получено %q", "mieru: protocol must be TCP or UDP, got %q", pr)
			}
		}
		u, _ := url.Parse(raw)
		if u == nil || u.User == nil || u.User.Username() == "" {
			return nil, E("mieru: нет имени пользователя", "mieru: no user name")
		}
		if _, ok := u.User.Password(); !ok {
			return nil, E("mieru: нет пароля", "mieru: no password")
		}
		p2.Port = strings.Join(ports, ",")
	case "naive":
		if _, err := NaiveArgs(raw, 1); err != nil {
			return nil, err
		}
	case "vless", "trojan", "tuic", "anytls":
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", typ, err)
		}
		if u.User == nil || u.User.Username() == "" {
			return nil, Ef("%s: нет UUID/пароля перед @", "%s: no UUID/password before @", typ)
		}
	case "hysteria2":
		if !strings.Contains(body, "@") {
			return nil, E("hysteria2: нет пароля (auth) перед @", "hysteria2: no password (auth) before @")
		}
	}
	return p2, nil
}

// parseGeneric выдирает host/port/fragment без строгой валидации порта
// (у hy2 бывает "host:443,20000-30000").
func parseGeneric(p *ParsedURI, body string) (*ParsedURI, error) {
	if i := strings.IndexByte(body, '#'); i >= 0 {
		if n, err := url.PathUnescape(body[i+1:]); err == nil {
			p.Name = n
		} else {
			p.Name = body[i+1:]
		}
		body = body[:i]
	}
	auth := body
	if i := strings.IndexAny(auth, "/?"); i >= 0 {
		auth = auth[:i]
	}
	if i := strings.LastIndexByte(auth, '@'); i >= 0 {
		auth = auth[i+1:]
	}
	host, port := auth, ""
	if strings.HasPrefix(auth, "[") {
		if i := strings.IndexByte(auth, ']'); i > 0 {
			host = auth[1:i]
			rest := auth[i+1:]
			port = strings.TrimPrefix(rest, ":")
		}
	} else if i := strings.IndexByte(auth, ':'); i >= 0 {
		host, port = auth[:i], auth[i+1:]
	}
	if host == "" {
		return nil, E("не указан адрес сервера", "no server address")
	}
	p.Host, p.Port = host, port
	return p, nil
}

func queryOf(raw string) url.Values {
	s := raw
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		q, _ := url.ParseQuery(s[i+1:])
		return q
	}
	return url.Values{}
}

func decodeVmess(body string) (map[string]any, error) {
	b := strings.TrimRight(body, "=")
	var buf []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		if buf, err = enc.DecodeString(b); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// WithName возвращает ссылку, где имя прокси заменено на name.
func WithName(raw, name string) string {
	raw = strings.TrimSpace(raw)
	scheme, body, _ := strings.Cut(raw, "://")
	if strings.EqualFold(scheme, "vmess") {
		if m, err := decodeVmess(body); err == nil {
			m["ps"] = name
			if b, err := json.Marshal(m); err == nil {
				return "vmess://" + base64.StdEncoding.EncodeToString(b)
			}
		}
	}
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	return raw + "#" + url.PathEscape(name)
}

// ProxyNames — какие имена появятся в mihomo для этого сервера.
// mieru-ссылка с несколькими port/protocol даёт несколько прокси "имя:порт/протокол".
func ProxyNames(s *Server) []string {
	if s.Type == "mieru" {
		q := queryOf(s.URI)
		ports, protos := q["port"], q["protocol"]
		out := make([]string, 0, len(ports))
		for i := range ports {
			if i < len(protos) {
				out = append(out, fmt.Sprintf("%s:%s/%s", s.Name, ports[i], protos[i]))
			}
		}
		return out
	}
	return []string{s.Name}
}

// NaiveArgs строит аргументы запуска naive для ссылки naive+https:// / naive+quic://
// Поддерживаемые query: padding (игнорируется, naive сам), extra-headers,
// insecure-concurrency / concurrency.
func NaiveArgs(raw string, localPort int) ([]string, error) {
	raw = strings.TrimSpace(raw)
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return nil, E("naive: неверная ссылка", "naive: bad link")
	}
	proto := strings.TrimPrefix(strings.ToLower(scheme), "naive+")
	if proto != "https" && proto != "quic" {
		return nil, E("naive: схема должна быть naive+https:// или naive+quic://", "naive: scheme must be naive+https:// or naive+quic://")
	}
	u, err := url.Parse(proto + "://" + rest)
	if err != nil {
		return nil, fmt.Errorf("naive: %v", err)
	}
	if u.Hostname() == "" {
		return nil, E("naive: нет адреса сервера", "naive: no server address")
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, E("naive: нет логина:пароля перед @", "naive: no login:password before @")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, Ef("naive: неверный порт %q", "naive: bad port %q", p)
		}
	}
	q := u.Query()
	proxy := url.URL{Scheme: proto, User: u.User, Host: u.Host}
	args := []string{
		"--listen=socks://127.0.0.1:" + strconv.Itoa(localPort),
		"--proxy=" + proxy.String(),
	}
	if c := firstNonEmpty(q.Get("insecure-concurrency"), q.Get("concurrency")); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n > 0 && n <= 8 {
			args = append(args, "--insecure-concurrency="+strconv.Itoa(n))
		}
	}
	if h := q.Get("extra-headers"); h != "" {
		args = append(args, "--extra-headers="+h)
	}
	return args, nil
}

func NaiveHost(raw string) string {
	_, rest, _ := strings.Cut(strings.TrimSpace(raw), "://")
	u, err := url.Parse("https://" + rest)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func isIP(h string) bool { return net.ParseIP(h) != nil }
