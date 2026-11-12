package main

// Консольное управление: mihomo-web menu (интерактивно) и разовые команды
// status / list / use / mode / test / conns / logs / add.
// Ходит в работающую панель через unix-сокет (WEB_SOCKET), пароль не нужен.
// Запуск на RouterOS: /container/shell <контейнер>, затем `vpn`.
// Язык: в терминале RouterOS (только ASCII) — английский, с --utf8 — русский.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var cliCommands = map[string]bool{"menu": true, "tui": true, "status": true, "list": true, "use": true,
	"mode": true, "test": true, "conns": true, "logs": true, "add": true, "help": true, "refresh": true}

func isCLI(cmd string) bool { return cliCommands[cmd] }

// L — строка интерфейса: английская в ASCII-режиме (терминал RouterOS), иначе русская.
func L(ru, en string) string {
	if asciiOut {
		return en
	}
	return ru
}

type cli struct {
	hc    *http.Client
	base  string
	user  string
	pass  string
	color bool
	in    chan string // строки stdin; закрыт при EOF
}

func runCLI(cfg Config, cmd string, args []string) int {
	c := newCLI(cfg) // цвет решаем по настоящему stdout, до подмены фильтром
	var forceUTF8, forceASCII bool
	rest := args[:0:0]
	for _, a := range args {
		switch a {
		case "--utf8", "-u", "--utf-8", "--ru":
			forceUTF8 = true
		case "--ascii", "-a", "--en":
			forceASCII = true
		default:
			rest = append(rest, a)
		}
	}
	args = rest
	restore := setupOutput(forceUTF8, forceASCII)
	defer restore()
	var err error
	switch cmd {
	case "menu", "tui":
		err = c.menu()
	case "status":
		err = c.cmdStatus()
	case "list":
		err = c.cmdList()
	case "use":
		if len(args) == 0 {
			err = errors.New(L("укажите номер или имя сервера: use 2", "give a server number or name: use 2"))
			break
		}
		err = c.cmdUse(strings.Join(args, " "))
	case "mode":
		if len(args) == 0 {
			err = errors.New(L("укажите режим: mode auto|fallback|manual|balance|direct", "give a mode: mode auto|fallback|manual|balance|direct"))
			break
		}
		err = c.cmdMode(args[0])
	case "test":
		err = c.cmdTest(strings.Join(args, " "))
	case "conns":
		err = c.cmdConns(strings.Join(args, " "), 50)
	case "logs":
		follow := len(args) > 0 && (args[0] == "-f" || args[0] == "follow")
		if follow {
			args = args[1:]
		}
		err = c.cmdLogs(strings.Join(args, " "), 50, follow)
	case "add":
		b, _ := io.ReadAll(os.Stdin)
		err = c.add(string(b))
	case "refresh":
		if len(args) == 0 {
			err = c.refreshAll()
			break
		}
		err = c.cmdRefresh(strings.Join(args, " "))
	case "help":
		c.help()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, c.red(L("ошибка: ", "error: "))+err.Error())
		return 1
	}
	return 0
}

func newCLI(cfg Config) *cli {
	c := &cli{color: isTerminal() && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"}
	if st, err := os.Stat(cfg.Socket); err == nil && st.Mode()&os.ModeSocket != 0 {
		sock := cfg.Socket
		c.base = "http://console"
		c.hc = &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			}}}
		return c
	}
	// запасной путь — TCP с паролем панели (env контейнера или окружение PID 1)
	port := cfg.Listen[strings.LastIndex(cfg.Listen, ":")+1:]
	c.base = "http://127.0.0.1:" + port
	c.hc = &http.Client{Timeout: 90 * time.Second}
	c.user, c.pass = cfg.User, cfg.Password
	if c.pass == "" {
		pe := pid1Env()
		c.user = firstNonEmpty(pe["WEB_USER"], c.user)
		c.pass = firstNonEmpty(pe["WEB_PASSWORD"], pe["UI_SECRET"])
	}
	return c
}

func pid1Env() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile("/proc/1/environ")
	if err != nil {
		return out
	}
	for _, kv := range bytes.Split(b, []byte{0}) {
		if k, v, ok := strings.Cut(string(kv), "="); ok {
			out[k] = v
		}
	}
	return out
}

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// ---------- HTTP ----------

func (c *cli) call(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Mihomo-Web", "1")
	if asciiOut {
		req.Header.Set("Accept-Language", "en") // ошибки сервера — по-английски
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", L("панель не отвечает (контейнер запущен? WEB_ENABLE=true?)",
			"panel is not responding (container running? WEB_ENABLE=true?)"), err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error  string   `json:"error"`
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(data, &e)
		msg := firstNonEmpty(e.Error, strings.Join(e.Errors, "; "), strings.TrimSpace(string(data)))
		return fmt.Errorf("%s (HTTP %d)", msg, resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type cliState struct {
	Version string `json:"version"`
	Mihomo  struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Error   string `json:"error"`
	} `json:"mihomo"`
	Mode        string       `json:"mode"`
	Manual      string       `json:"manual"`
	Current     string       `json:"current"`
	Placeholder string       `json:"placeholder"`
	Servers     []serverView `json:"servers"`
}

func (c *cli) state() (*cliState, error) {
	var s cliState
	return &s, c.call(http.MethodGet, "/api/state", nil, &s)
}

func (c *cli) activity(n int) (*Activity, error) {
	var a Activity
	return &a, c.call(http.MethodGet, "/api/activity?recent="+strconv.Itoa(n), nil, &a)
}

// ---------- оформление ----------

func (c *cli) paint(code, s string) string {
	if !c.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
func (c *cli) bold(s string) string   { return c.paint("1", s) }
func (c *cli) dim(s string) string    { return c.paint("2", s) }
func (c *cli) red(s string) string    { return c.paint("31", s) }
func (c *cli) green(s string) string  { return c.paint("32", s) }
func (c *cli) yellow(s string) string { return c.paint("33", s) }
func (c *cli) cyan(s string) string   { return c.paint("36", s) }

func (c *cli) clear() {
	if c.color {
		fmt.Print("\x1b[H\x1b[2J")
	} else {
		fmt.Println()
	}
}

// значки: в ASCII-режиме — ASCII-замены
func sym(u, a string) string { return L(u, a) }

// pad/cut по числу символов, а не байт (кириллица).
func pad(s string, w int) string {
	if asciiOut {
		s = translit(s) // имена серверов по-русски — транслит; ширина после него
	}
	n := utf8.RuneCountInString(s)
	if n > w {
		r := []rune(s)
		if w > 1 {
			return string(r[:w-1]) + sym("…", "~")
		}
		return string(r[:w])
	}
	return s + strings.Repeat(" ", w-n)
}

func padL(s string, w int) string {
	if asciiOut {
		s = translit(s)
	}
	n := utf8.RuneCountInString(s)
	if n >= w {
		return s
	}
	return strings.Repeat(" ", w-n) + s
}

func modeName(m string) string {
	switch m {
	case "AUTO":
		return L("авто", "auto")
	case "FALLBACK":
		return L("резерв", "fallback")
	case "MANUAL":
		return L("вручную", "manual")
	case "LOADBALANCE":
		return L("балансировка", "load balance")
	case "DIRECT":
		return L("без VPN", "no VPN")
	}
	return m
}

func modeLong(m string) string {
	switch m {
	case "AUTO":
		return L("авто (самый быстрый)", "auto (fastest)")
	case "FALLBACK":
		return L("резерв по порядку", "fallback in list order")
	}
	return modeName(m)
}

func modeArg(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "auto", "авто", "a":
		return "AUTO", true
	case "2", "fallback", "резерв", "f":
		return "FALLBACK", true
	case "3", "manual", "вручную", "m":
		return "MANUAL", true
	case "4", "loadbalance", "balance", "балансировка", "b":
		return "LOADBALANCE", true
	case "5", "direct", "напрямую", "d":
		return "DIRECT", true
	}
	return "", false
}

func delayText(d int) string {
	switch {
	case d < 0:
		return sym("—", "-")
	case d == 0:
		return L("нет связи", "no link")
	}
	return strconv.Itoa(d) + L(" мс", " ms")
}

func (c *cli) delayColored(d int) string {
	t := delayText(d)
	switch {
	case d == 0:
		return c.red(t)
	case d > 400:
		return c.yellow(t)
	case d > 0:
		return c.green(t)
	}
	return c.dim(t)
}

func rateHuman(bps int64) string {
	bits := float64(bps) * 8
	switch {
	case bits >= 1e6:
		return fmt.Sprintf("%.1f %s", bits/1e6, L("Мбит/с", "Mbit/s"))
	case bits >= 1e3:
		return fmt.Sprintf("%.0f %s", bits/1e3, L("кбит/с", "kbit/s"))
	}
	return fmt.Sprintf("%.0f %s", bits, L("бит/с", "bit/s"))
}

func bytesHuman(b int64) string {
	f := float64(b)
	switch {
	case f >= 1<<30:
		return fmt.Sprintf("%.2f %s", f/(1<<30), L("ГБ", "GB"))
	case f >= 1<<20:
		return fmt.Sprintf("%.1f %s", f/(1<<20), L("МБ", "MB"))
	case f >= 1<<10:
		return fmt.Sprintf("%.0f %s", f/(1<<10), L("КБ", "KB"))
	}
	return fmt.Sprintf("%d %s", b, L("Б", "B"))
}

// eventText — событие монитора на языке консоли.
func eventText(e Event) string {
	if asciiOut && e.MsgEN != "" {
		return e.MsgEN
	}
	return e.Msg
}

// ---------- экраны ----------

func (c *cli) printHeader(s *cliState, a *Activity) {
	dot := sym(" · ", " | ")
	if s.Mihomo.OK {
		fmt.Printf("%s  mihomo %s %s%s%s %s\n", c.bold("VPN"), s.Mihomo.Version, c.green(L("работает", "running")), dot, L("панель", "panel"), s.Version)
	} else {
		fmt.Printf("%s  mihomo %s: %s\n", c.bold("VPN"), c.red(L("не отвечает", "not responding")), s.Mihomo.Error)
	}
	via, delay := "", -1
	if a != nil && a.Via != "" {
		via, delay = a.Via, a.ViaDelay
		if !a.ViaAlive && delay > 0 {
			delay = 0
		}
	} else {
		via = firstNonEmpty(s.Current, s.Manual)
	}
	if via == s.Placeholder {
		via = L("нет включённых серверов — напрямую", "no enabled servers - direct")
	}
	if s.Mode == "DIRECT" {
		via = L("напрямую, без VPN", "direct, no VPN")
	}
	if asciiOut {
		via = translit(enName(via))
	}
	fmt.Printf("%s %s  %s  %s\n", L("Трафик через:", "Traffic via:"), c.bold(firstNonEmpty(via, sym("—", "-"))), c.delayColored(delay),
		c.dim(L("режим: ", "mode: ")+modeName(s.Mode)))
	if a != nil && a.OK {
		fmt.Printf("%s %s %s  %s %s  %s %d%s%s %s %s %s %s\n",
			L("Сейчас:", "Now:"), sym("↓", "down"), c.cyan(rateHuman(a.DownRate)), sym("↑", "up"), c.cyan(rateHuman(a.UpRate)),
			L("соединений", "connections"), a.Conns, dot, L("всего", "total"),
			sym("↓", "down"), bytesHuman(a.DnTotal), sym("↑", "up"), bytesHuman(a.UpTotal))
	}
}

func (c *cli) printServers(s *cliState) {
	if len(s.Servers) == 0 {
		fmt.Println(c.dim(L("  Серверов в панели нет (серверы из env SRV/SUB здесь не показываются).",
			"  No servers in the panel (servers from env SRV/SUB are not listed here).")))
		return
	}
	fmt.Println(c.dim(" #  " + pad("", 2) + pad(L("Имя", "Name"), 20) + " " + pad(L("Тип", "Type"), 7) + " " +
		pad(L("Адрес", "Address"), 30) + " " + padL(L("Пинг", "Ping"), 10)))
	for i, x := range s.Servers {
		if x.Type == "sub" {
			c.printSubRow(i+1, x)
			continue
		}
		var mark string
		switch {
		case !x.Enabled:
			mark = c.dim(sym("·", "-"))
		case !x.Loaded:
			mark = c.red(sym("✕", "x"))
		case x.Delay == 0:
			mark = c.red(sym("●", "*"))
		case x.Delay > 0:
			mark = c.green(sym("●", "*"))
		default:
			mark = c.yellow(sym("●", "?"))
		}
		name := pad(x.Name, 20)
		if x.Parent != "" { // сервер из подписки — с отступом
			name = sym(" └ ", " `-") + pad(x.Name, 17)
		}
		if x.Active {
			name = c.bold(name)
		}
		addr := x.Host
		if x.Port != "" {
			addr += ":" + x.Port
		}
		ping := padL(delayText(x.Delay), 10)
		if x.Enabled {
			switch {
			case x.Delay == 0:
				ping = c.red(ping)
			case x.Delay > 0:
				ping = c.green(ping)
			}
		} else {
			ping = c.dim(padL(L("выкл", "off"), 10))
		}
		note := ""
		if x.Active {
			note = c.cyan(" " + sym("←", "<-") + " " + L("сейчас", "active"))
		}
		if x.Type == "naive" && x.Naive != nil && x.Naive.State != "running" && x.Enabled {
			note += c.yellow(" naive: " + x.Naive.State)
		}
		if x.Enabled && !x.Loaded && s.Mihomo.OK {
			note += c.red(L(" ссылка не принята", " link rejected by mihomo"))
		}
		fmt.Printf("%2d  %s %s %s %s %s%s\n", i+1, mark, name, pad(firstNonEmpty(typeShort[x.Type], x.Type), 7), pad(addr, 30), ping, note)
	}
}

// printSubRow — строка подписки: сколько серверов, когда обновлена, ошибка, остаток трафика.
func (c *cli) printSubRow(n int, x serverView) {
	mark := c.cyan(sym("◆", "#"))
	if !x.Enabled {
		mark = c.dim(sym("·", "-"))
	} else if x.Sub != nil && x.Sub.Error != "" {
		mark = c.red(sym("◆", "#"))
	}
	count := padL(fmt.Sprintf(L("%d серв.", "%d srv"), x.Count), 10)
	if !x.Enabled {
		count = c.dim(padL(L("выкл", "off"), 10))
	}
	note := ""
	if x.Sub != nil {
		if !x.Sub.UpdatedAt.IsZero() {
			note += c.dim(" " + L("обновлена ", "updated ") + x.Sub.UpdatedAt.Local().Format("02.01 15:04"))
		}
		if x.Sub.Total > 0 {
			note += c.dim(fmt.Sprintf(" %s %s/%s", L("трафик", "traffic"), bytesHuman(x.Sub.Upload+x.Sub.Download), bytesHuman(x.Sub.Total)))
		}
		if x.Sub.Expire > 0 {
			note += c.dim(" " + L("до ", "until ") + time.Unix(x.Sub.Expire, 0).Format("02.01.2006"))
		}
		if e := firstNonEmpty(L(x.Sub.Error, x.Sub.ErrorEN), x.Sub.Error); e != "" {
			note += c.red(" " + e)
		}
	}
	fmt.Printf("%2d  %s %s %s %s %s%s\n", n, mark, c.bold(pad(x.Name, 20)), pad(L("Подп.", "SUB"), 7), pad(x.Host, 30), count, note)
}

var typeShort = map[string]string{"vless": "VLESS", "hysteria2": "HY2", "mieru": "Mieru", "naive": "Naive", "vmess": "VMess",
	"trojan": "Trojan", "ss": "SS", "tuic": "TUIC", "anytls": "AnyTLS", "hysteria": "HY1", "socks5": "SOCKS5"}

func (c *cli) printRecent(a *Activity, n int) {
	if len(a.Recent) == 0 {
		fmt.Println(c.dim(L("  новых соединений пока нет", "  no new connections yet")))
		return
	}
	for i, r := range a.Recent {
		if i >= n {
			break
		}
		fmt.Println("  " + c.connLine(r))
	}
}

func (c *cli) printEvents(a *Activity, n int) {
	if len(a.Events) == 0 {
		return
	}
	fmt.Println(c.dim(L("События:", "Events:")))
	for i, e := range a.Events {
		if i >= n {
			break
		}
		fmt.Printf("  %s %s\n", c.dim(e.T.Local().Format("15:04:05")), eventText(e))
	}
}

func (c *cli) connLine(r ConnView) string {
	dst := firstNonEmpty(r.Host, r.DstIP)
	if r.Port != "" {
		dst += ":" + r.Port
	}
	via := r.Via
	if asciiOut {
		via = translit(enName(via))
	}
	switch {
	case r.Internal:
		via = c.dim(L("служебное", "internal"))
	case r.Rejected:
		via = c.red(via)
	case r.Direct:
		via = c.yellow(via)
	default:
		via = c.green(via)
	}
	return fmt.Sprintf("%s %-3s %-15s %s %s %s %s", c.dim(r.Start.Local().Format("15:04:05")), r.Net, r.Src, sym("→", "->"),
		pad(dst, 40), c.dim(L("через", "via")), via)
}

func (c *cli) cmdStatus() error {
	s, err := c.state()
	if err != nil {
		return err
	}
	a, _ := c.activity(10)
	c.printHeader(s, a)
	if a != nil {
		c.printEvents(a, 5)
		fmt.Println(c.dim(L("Последние запросы:", "Recent requests:")))
		c.printRecent(a, 10)
	}
	return nil
}

func (c *cli) cmdList() error {
	s, err := c.state()
	if err != nil {
		return err
	}
	c.printServers(s)
	return nil
}

// pick — сервер по номеру из списка или по имени.
func (c *cli) pick(arg string) (*serverView, error) {
	s, err := c.state()
	if err != nil {
		return nil, err
	}
	arg = strings.TrimSpace(arg)
	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(s.Servers) {
			return nil, fmt.Errorf(L("нет сервера №%d", "no server #%d"), n)
		}
		return &s.Servers[n-1], nil
	}
	for i := range s.Servers {
		if strings.EqualFold(s.Servers[i].Name, arg) || (asciiOut && strings.EqualFold(translit(s.Servers[i].Name), arg)) {
			return &s.Servers[i], nil
		}
	}
	return nil, fmt.Errorf(L("сервер «%s» не найден", "server %q not found"), arg)
}

// shown — имя сервера для вывода в тексте (в ASCII-режиме транслитом).
func shown(name string) string {
	if asciiOut {
		return "\"" + translit(name) + "\""
	}
	return "«" + name + "»"
}

func (c *cli) cmdUse(arg string) error {
	x, err := c.pick(arg)
	if err != nil {
		return err
	}
	var r struct {
		Proxy string `json:"proxy"`
	}
	if err := c.call(http.MethodPost, "/api/servers/"+x.ID+"/use", nil, &r); err != nil {
		return err
	}
	fmt.Println(c.green(sym("✓ ", "+ ")) + L("трафик идёт через ", "traffic now goes via ") + shown(firstNonEmpty(r.Proxy, x.Name)) +
		L(" (режим «вручную»)", " (mode: manual)"))
	return nil
}

func (c *cli) cmdMode(arg string) error {
	m, ok := modeArg(arg)
	if !ok {
		return fmt.Errorf(L("неизвестный режим «%s»", "unknown mode %q"), arg)
	}
	if err := c.call(http.MethodPost, "/api/mode", map[string]string{"mode": m}, nil); err != nil {
		return err
	}
	fmt.Println(c.green(sym("✓ ", "+ ")) + L("режим: ", "mode: ") + modeName(m))
	return nil
}

func (c *cli) cmdTest(arg string) error {
	if strings.TrimSpace(arg) == "" {
		fmt.Println(L("Проверяю все серверы…", "Testing all servers..."))
		if err := c.call(http.MethodPost, "/api/test-all", nil, nil); err != nil {
			return err
		}
		return c.cmdList()
	}
	x, err := c.pick(arg)
	if err != nil {
		return err
	}
	var r struct {
		Delay int `json:"delay"`
	}
	fmt.Printf("%s %s%s\n", L("Проверяю", "Testing"), shown(x.Name), sym("…", "..."))
	if err := c.call(http.MethodPost, "/api/servers/"+x.ID+"/test", nil, &r); err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", shown(x.Name), c.delayColored(r.Delay))
	return nil
}

func (c *cli) cmdConns(filter string, limit int) error {
	var r struct {
		Connections []ConnView `json:"connections"`
	}
	if err := c.call(http.MethodGet, "/api/connections", nil, &r); err != nil {
		return err
	}
	f := strings.ToLower(strings.TrimSpace(filter))
	byVia := map[string]int{}
	shownN := 0
	for _, x := range r.Connections {
		key := x.Via
		if x.Internal {
			key = L("служебные", "internal")
		}
		byVia[key]++
		if f != "" && !strings.Contains(strings.ToLower(x.Host+" "+x.DstIP+" "+x.Src+" "+x.Via), f) {
			continue
		}
		if shownN < limit {
			fmt.Printf("  %s  %s\n", c.connLine(x), c.dim(sym("↓", "down ")+bytesHuman(x.Down)+" "+sym("↑", "up ")+bytesHuman(x.Up)))
		}
		shownN++
	}
	parts := []string{}
	for k, n := range byVia {
		if asciiOut {
			k = translit(k)
		}
		parts = append(parts, fmt.Sprintf("%s: %d", k, n))
	}
	sort.Strings(parts)
	fmt.Printf("%s %d", L("Всего соединений", "Total connections"), len(r.Connections))
	if len(parts) > 0 {
		fmt.Printf(" (%s)", strings.Join(parts, ", "))
	}
	if f != "" {
		fmt.Printf(L(", по фильтру «%s»: %d", ", matching %q: %d"), filter, shownN)
	}
	if shownN > limit {
		fmt.Printf(L(", показаны первые %d", ", first %d shown"), limit)
	}
	fmt.Println()
	return nil
}

func (c *cli) fetchLogs(after int64, limit int, q string) ([]LogLine, error) {
	var r struct {
		Lines []LogLine `json:"lines"`
	}
	v := url.Values{}
	v.Set("after", strconv.FormatInt(after, 10))
	v.Set("limit", strconv.Itoa(limit))
	if q != "" {
		v.Set("q", q)
	}
	err := c.call(http.MethodGet, "/api/logs?"+v.Encode(), nil, &r)
	return r.Lines, err
}

func (c *cli) logLine(l LogLine) string {
	t := c.dim(l.T.Local().Format("15:04:05"))
	switch l.Level {
	case "error":
		return t + " " + c.red(l.Text)
	case "warning":
		return t + " " + c.yellow(l.Text)
	}
	return t + " " + l.Text
}

// cmdLogs — журнал mihomo; follow: дописывать новые строки до Enter (или Ctrl+C вне меню).
func (c *cli) cmdLogs(q string, n int, follow bool) error {
	lines, err := c.fetchLogs(0, n, q)
	if err != nil {
		return err
	}
	var seq int64
	for _, l := range lines {
		fmt.Println(c.logLine(l))
		seq = l.Seq
	}
	if len(lines) == 0 {
		if q != "" {
			fmt.Println(c.dim(fmt.Sprintf(L("по фильтру «%s» ничего нет", "nothing matches %q"), q)))
		} else {
			fmt.Println(c.dim(L("журнал пуст", "log is empty")))
		}
	}
	if !follow {
		return nil
	}
	if seq == 0 { // пустой фильтрованный вывод — начинаем с текущего конца
		if all, err := c.fetchLogs(0, 1, ""); err == nil && len(all) > 0 {
			seq = all[len(all)-1].Seq
		}
	}
	fmt.Println(c.dim(L("— новые строки; Enter — выход —", "-- new lines; Enter to stop --")))
	stop, cancel := c.stopper()
	defer cancel()
	for {
		select {
		case <-stop:
			return nil
		case <-time.After(time.Second):
		}
		lines, err := c.fetchLogs(seq, 200, q)
		if err != nil {
			return err
		}
		for _, l := range lines {
			fmt.Println(c.logLine(l))
			seq = l.Seq
		}
		if q != "" && len(lines) == 0 {
			if all, err := c.fetchLogs(seq, 500, ""); err == nil && len(all) > 0 {
				seq = all[len(all)-1].Seq
			}
		}
	}
}

// stopper — канал, который закрывается по Enter. cancel() снимает ожидание, не съедая
// следующую строку меню (важно, если экран закрылся по ошибке, а не по Enter).
func (c *cli) stopper() (<-chan struct{}, func()) {
	ch, done := make(chan struct{}), make(chan struct{})
	if c.in != nil {
		go func() {
			select {
			case <-c.in:
				close(ch)
			case <-done:
			}
		}()
		return ch, func() { close(done) }
	}
	go func() {
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		close(ch)
	}()
	return ch, func() {}
}

func (c *cli) watch() error {
	stop, cancel := c.stopper()
	defer cancel()
	for {
		s, err := c.state()
		if err != nil {
			return err
		}
		a, err := c.activity(15)
		if err != nil {
			return err
		}
		c.clear()
		c.printHeader(s, a)
		fmt.Println(c.sparkline(a.History))
		c.printEvents(a, 3)
		fmt.Println(c.dim(L("Последние запросы:", "Recent requests:")))
		c.printRecent(a, 15)
		fmt.Println(c.dim(L("— обновление каждые 2 с; Enter — назад —", "-- refresh every 2 s; Enter to go back --")))
		select {
		case <-stop:
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *cli) sparkline(h []RatePoint) string {
	if len(h) == 0 {
		return ""
	}
	if len(h) > 60 {
		h = h[len(h)-60:]
	}
	bars := []rune(sym("▁▂▃▄▅▆▇█", "_.:-=+*#"))
	var mx int64 = 1
	for _, p := range h {
		if p.Down > mx {
			mx = p.Down
		}
	}
	var b strings.Builder
	for _, p := range h {
		i := int(p.Down * int64(len(bars)-1) / mx)
		b.WriteRune(bars[i])
	}
	mins := strconv.Itoa(len(h) * 5 / 60)
	return c.dim(L("↓ за "+mins+" мин: ", "down, last "+mins+" min: ")) + c.cyan(b.String()) + c.dim(L(" пик ", " peak ")+rateHuman(mx))
}

func (c *cli) add(text string) error {
	var r struct {
		Added   int      `json:"added"`
		Errors  []string `json:"errors"`
		Warning string   `json:"warning"`
		Subs    []string `json:"subs"`
	}
	err := c.call(http.MethodPost, "/api/servers", map[string]string{"text": text}, &r)
	if r.Added > 0 {
		fmt.Printf("%s %s %d\n", c.green(sym("✓", "+")), L("добавлено:", "added:"), r.Added)
	}
	for _, s := range r.Subs {
		fmt.Printf("  %s %s\n", L("подписка", "subscription"), s+L(" серв.", " servers"))
	}
	for _, e := range r.Errors {
		fmt.Println(c.red(sym("  ✕ ", "  x ")) + e)
	}
	if r.Warning != "" {
		fmt.Println(c.yellow(L("  mihomo не перечитал список: ", "  mihomo did not reload the list: ")) + r.Warning)
	}
	if err != nil && r.Added == 0 && len(r.Errors) == 0 {
		return err
	}
	return nil
}

// ---------- интерактивное меню ----------

func (c *cli) help() {
	fmt.Print(L(`Команды (N — номер сервера из списка):
  Enter / r   обновить              w         монитор: скорость и запросы вживую
  u N         использовать N        s [текст] текущие соединения (фильтр)
  c N         проверить N           j [текст] журнал mihomo (фильтр)
  t           проверить все         f [текст] журнал вживую
  m           режим (авто/резерв/вручную/балансировка/без VPN)
  a           добавить (ссылки на серверы или https://… подписку)
  e N         изменить N            o N       включить/выключить N
  k N         поднять N выше        d N       удалить N
  p N         обновить подписку N   l N       журнал naive N
  b           бэкап (JSON на экран) q         выход
Серверы из подписки (с отступом) только используются и проверяются; менять — подписку целиком.
Разовые команды: vpn status | list | use N | mode auto | test [N] | refresh [N] | conns [текст] | logs [-f] [текст]
Язык: в терминале RouterOS (только ASCII) — английский; vpn --en / --ru — выбрать явно.
`, `Commands (N = server number from the list):
  Enter / r   refresh               w         live monitor: speed and requests
  u N         use server N          s [text]  current connections (filter)
  c N         test server N         j [text]  mihomo log (filter)
  t           test all              f [text]  follow mihomo log
  m           mode (auto / fallback / manual / load balance / no VPN)
  a           add (server links or an https://... subscription)
  e N         edit N                o N       enable / disable N
  k N         move N up             d N       delete N
  p N         refresh subscription  l N       naive log of N
  b           backup (JSON)         q         quit
Servers from a subscription (indented) can only be used and tested; edit the subscription itself.
One-shot: vpn status | list | use N | mode auto | test [N] | refresh [N] | conns [text] | logs [-f] [text]
Language: English in the RouterOS terminal (ASCII only); vpn --ru for Russian in a UTF-8 terminal.
`))
}

func (c *cli) readLine(prompt string) (string, bool) {
	fmt.Print(prompt)
	l, ok := <-c.in
	return strings.TrimSpace(l), ok
}

func (c *cli) menu() error {
	c.in = make(chan string)
	go func() {
		r := bufio.NewReader(os.Stdin)
		for {
			l, err := r.ReadString('\n')
			if l != "" || err == nil {
				c.in <- strings.TrimRight(l, "\r\n")
			}
			if err != nil {
				close(c.in)
				return
			}
		}
	}()
	msg := ""
	for {
		s, err := c.state()
		if err != nil {
			return err
		}
		a, _ := c.activity(5)
		c.clear()
		c.printHeader(s, a)
		fmt.Println()
		c.printServers(s)
		fmt.Println()
		if msg != "" {
			fmt.Println(msg)
		}
		fmt.Println(c.dim(L("u N использовать · c N проверить · t все · m режим · a добавить · p N обновить подписку · e/o/d/k N · w монитор · s соединения · j журнал · ? помощь · q выход",
			"u N use | c N test | t test all | m mode | a add | p N refresh sub | e/o/d/k N | w monitor | s conns | j log | ? help | q quit")))
		line, ok := c.readLine("> ")
		if !ok {
			fmt.Println()
			return nil
		}
		msg = ""
		cmd, arg, _ := strings.Cut(line, " ")
		arg = strings.TrimSpace(arg)
		var cerr error
		switch strings.ToLower(cmd) {
		case "", "r":
		case "q", "quit", "exit", "й":
			return nil
		case "?", "h", "help":
			c.clear()
			c.help()
			c.pause()
		case "u":
			cerr = c.withArg(arg, c.cmdUse)
		case "c":
			cerr = c.withArg(arg, c.cmdTest)
			c.pause()
		case "t":
			fmt.Println(L("Проверяю все…", "Testing all..."))
			cerr = c.call(http.MethodPost, "/api/test-all", nil, nil)
			if cerr == nil {
				msg = c.green(L("✓ проверено", "+ tested"))
			}
		case "m":
			cerr = c.menuMode(s.Mode)
		case "a":
			cerr = c.menuAdd()
			c.pause()
		case "e":
			cerr = c.withArg(arg, c.menuEdit)
		case "o":
			cerr = c.withArg(arg, func(a string) error {
				x, err := c.pick(a)
				if err != nil {
					return err
				}
				en := !x.Enabled
				return c.call(http.MethodPut, "/api/servers/"+x.ID, map[string]any{"enabled": en}, nil)
			})
		case "d":
			cerr = c.withArg(arg, func(a string) error {
				x, err := c.pick(a)
				if err != nil {
					return err
				}
				if l, _ := c.readLine(fmt.Sprintf(L("Удалить %s? (да/нет) ", "Delete %s? (yes/no) "), shown(x.Name))); !isYes(l) {
					return nil
				}
				return c.call(http.MethodDelete, "/api/servers/"+x.ID, nil, nil)
			})
		case "k":
			cerr = c.withArg(arg, func(a string) error { return c.moveUp(s, a) })
		case "p":
			cerr = c.withArg(arg, c.cmdRefresh)
			c.pause()
		case "l":
			cerr = c.withArg(arg, func(a string) error {
				x, err := c.pick(a)
				if err != nil {
					return err
				}
				var r struct {
					Lines []string `json:"lines"`
				}
				if err := c.call(http.MethodGet, "/api/servers/"+x.ID+"/log", nil, &r); err != nil {
					return err
				}
				c.clear()
				fmt.Println(c.bold(L("Журнал naive: ", "naive log: ") + shown(x.Name)))
				if len(r.Lines) == 0 {
					fmt.Println(c.dim(L("пусто — процесс не запускался (это не naive-сервер?)", "empty - process never started (not a naive server?)")))
				}
				for _, l := range r.Lines {
					fmt.Println(l)
				}
				c.pause()
				return nil
			})
		case "w":
			cerr = c.watch()
		case "s":
			c.clear()
			cerr = c.cmdConns(arg, 40)
			c.pause()
		case "j":
			c.clear()
			cerr = c.cmdLogs(arg, 40, false)
			c.pause()
		case "f":
			c.clear()
			cerr = c.cmdLogs(arg, 20, true)
		case "b":
			c.clear()
			var raw json.RawMessage
			if cerr = c.call(http.MethodGet, "/api/export", nil, &raw); cerr == nil {
				var buf bytes.Buffer
				_ = json.Indent(&buf, raw, "", "  ")
				fmt.Println(buf.String())
				fmt.Println(c.yellow(L("В бэкапе пароли серверов. Скопируйте и сохраните в надёжном месте.",
					"The backup contains server passwords. Copy it and keep it somewhere safe.")))
				c.pause()
			}
		default:
			msg = c.yellow(fmt.Sprintf(L("не понял «%s». ? — помощь", "unknown command %q. ? for help"), line))
		}
		if cerr != nil {
			msg = c.red(L("ошибка: ", "error: ")) + cerr.Error()
		}
	}
}

func isYes(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "да", "д", "y", "yes", "lf":
		return true
	}
	return false
}

func (c *cli) pause() { c.readLine(c.dim(L("Enter — назад ", "Enter to go back "))) }

func (c *cli) withArg(arg string, fn func(string) error) error {
	if arg == "" {
		l, ok := c.readLine(L("Номер сервера: ", "Server number: "))
		if !ok || l == "" {
			return nil
		}
		arg = l
	}
	return fn(arg)
}

func (c *cli) menuMode(cur string) error {
	fmt.Println(L("Режим:", "Mode:"))
	for i, k := range []string{"AUTO", "FALLBACK", "MANUAL", "LOADBALANCE", "DIRECT"} {
		mark := "  "
		if k == cur {
			mark = c.cyan(sym("→ ", "> "))
		}
		fmt.Printf("  %s%d  %s\n", mark, i+1, modeLong(k))
	}
	l, ok := c.readLine(L("Номер (пусто — отмена): ", "Number (empty = cancel): "))
	if !ok || l == "" {
		return nil
	}
	return c.cmdMode(l)
}

func (c *cli) menuAdd() error {
	fmt.Println(L("Вставьте ссылки по одной в строке (vless://, hy2://, mierus://, naive+https://…) или ссылку подписки https://….",
		"Paste links, one per line (vless://, hy2://, mierus://, naive+https://...) or a subscription link https://...."))
	fmt.Println(c.dim(L("Пустая строка — закончить.", "Empty line to finish.")))
	var lines []string
	for {
		l, ok := c.readLine("")
		if !ok || l == "" {
			break
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return nil
	}
	return c.add(strings.Join(lines, "\n"))
}

func (c *cli) menuEdit(arg string) error {
	x, err := c.pick(arg)
	if err != nil {
		return err
	}
	var full Server
	if err := c.call(http.MethodGet, "/api/export?id="+url.QueryEscape(x.ID), nil, &full); err != nil {
		return err
	}
	fmt.Printf("%s %s\n", L("Имя:", "Name:"), full.Name)
	name, ok := c.readLine(L("Новое имя (пусто — оставить): ", "New name (empty = keep): "))
	if !ok {
		return nil
	}
	fmt.Printf("%s %s\n", L("Ссылка:", "Link:"), c.dim(full.URI))
	uri, ok := c.readLine(L("Новая ссылка (пусто — оставить): ", "New link (empty = keep): "))
	if !ok {
		return nil
	}
	p := map[string]any{}
	if name != "" {
		p["name"] = name
	}
	if uri != "" {
		p["uri"] = uri
	}
	if len(p) == 0 {
		return nil
	}
	return c.call(http.MethodPut, "/api/servers/"+x.ID, p, nil)
}

// moveUp — поднять сервер или подписку на одну позицию среди строк верхнего уровня
// (серверы подписки двигаются вместе с ней).
func (c *cli) moveUp(s *cliState, arg string) error {
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > len(s.Servers) {
		return fmt.Errorf(L("укажите номер 1…%d", "give a number 1..%d"), len(s.Servers))
	}
	if s.Servers[n-1].Parent != "" {
		return errors.New(L("это сервер из подписки — двигайте подписку", "this server comes from a subscription - move the subscription"))
	}
	var ids []string
	pos := -1
	for _, x := range s.Servers {
		if x.Parent == "" {
			if x.ID == s.Servers[n-1].ID {
				pos = len(ids)
			}
			ids = append(ids, x.ID)
		}
	}
	if pos < 1 {
		return nil // уже первый
	}
	ids[pos-1], ids[pos] = ids[pos], ids[pos-1]
	return c.call(http.MethodPost, "/api/reorder", map[string]any{"ids": ids}, nil)
}

// cmdRefresh — обновить подписку (по номеру строки подписки или её имени).
func (c *cli) cmdRefresh(arg string) error {
	x, err := c.pick(arg)
	if err != nil {
		return err
	}
	if x.Type != "sub" {
		return errors.New(L("это не подписка", "not a subscription"))
	}
	fmt.Printf("%s %s%s\n", L("Обновляю", "Refreshing"), shown(x.Name), sym("…", "..."))
	var r struct {
		Count   int    `json:"count"`
		Warning string `json:"warning"`
	}
	if err := c.call(http.MethodPost, "/api/servers/"+x.ID+"/refresh", nil, &r); err != nil {
		return err
	}
	fmt.Printf("%s %s: %d%s\n", c.green(sym("✓", "+")), shown(x.Name), r.Count, L(" серв.", " servers"))
	if r.Warning != "" {
		fmt.Println(c.yellow(L("  mihomo не перечитал список: ", "  mihomo did not reload the list: ")) + r.Warning)
	}
	return nil
}

func (c *cli) refreshAll() error {
	s, err := c.state()
	if err != nil {
		return err
	}
	found := false
	for i, x := range s.Servers {
		if x.Type == "sub" && x.Enabled {
			found = true
			if err := c.cmdRefresh(strconv.Itoa(i + 1)); err != nil {
				fmt.Println(c.red(L("  ошибка: ", "  error: ")) + err.Error())
			}
		}
	}
	if !found {
		fmt.Println(L("Подписок нет.", "No subscriptions."))
	}
	return nil
}
