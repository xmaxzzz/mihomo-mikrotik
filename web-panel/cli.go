package main

// Консольное управление: mihomo-web menu (интерактивно) и разовые команды
// status / list / use / mode / test / conns / logs / add.
// Ходит в работающую панель через unix-сокет (WEB_SOCKET), пароль не нужен.
// Запуск на RouterOS: /container/shell <контейнер>, затем `vpn`.

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
	"mode": true, "test": true, "conns": true, "logs": true, "add": true, "help": true}

func isCLI(cmd string) bool { return cliCommands[cmd] }

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
		case "--utf8", "-u", "--utf-8":
			forceUTF8 = true
		case "--ascii", "-a":
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
			err = errors.New("укажите номер или имя сервера: use 2")
			break
		}
		err = c.cmdUse(strings.Join(args, " "))
	case "mode":
		if len(args) == 0 {
			err = errors.New("укажите режим: mode auto|fallback|manual|balance|direct")
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
	case "help":
		c.help()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, c.red("ошибка: ")+err.Error())
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("панель не отвечает (контейнер запущен? WEB_ENABLE=true?): %w", err)
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
func (c *cli) bold(s string) string  { return c.paint("1", s) }
func (c *cli) dim(s string) string   { return c.paint("2", s) }
func (c *cli) red(s string) string   { return c.paint("31", s) }
func (c *cli) green(s string) string { return c.paint("32", s) }
func (c *cli) yellow(s string) string {
	return c.paint("33", s)
}
func (c *cli) cyan(s string) string { return c.paint("36", s) }

func (c *cli) clear() {
	if c.color {
		fmt.Print("\x1b[H\x1b[2J")
	} else {
		fmt.Println()
	}
}

// pad/cut по числу символов, а не байт (кириллица).
func pad(s string, w int) string {
	if asciiOut {
		s = translit(s) // ширина — после транслита, иначе колонки поедут
	}
	n := utf8.RuneCountInString(s)
	if n > w {
		r := []rune(s)
		if w > 1 {
			if asciiOut {
				return string(r[:w-1]) + "~"
			}
			return string(r[:w-1]) + "…"
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

var modeRU = map[string]string{"AUTO": "авто", "FALLBACK": "резерв", "MANUAL": "вручную", "LOADBALANCE": "балансировка", "DIRECT": "без VPN"}

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
		return "—"
	case d == 0:
		return "нет связи"
	}
	return strconv.Itoa(d) + " мс"
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

func rateRU(bps int64) string {
	bits := float64(bps) * 8
	switch {
	case bits >= 1e6:
		return fmt.Sprintf("%.1f Мбит/с", bits/1e6)
	case bits >= 1e3:
		return fmt.Sprintf("%.0f кбит/с", bits/1e3)
	}
	return fmt.Sprintf("%.0f бит/с", bits)
}

func bytesRU(b int64) string {
	f := float64(b)
	switch {
	case f >= 1<<30:
		return fmt.Sprintf("%.2f ГБ", f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%.1f МБ", f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%.0f КБ", f/(1<<10))
	}
	return fmt.Sprintf("%d Б", b)
}

// ---------- экраны ----------

func (c *cli) printHeader(s *cliState, a *Activity) {
	if s.Mihomo.OK {
		fmt.Printf("%s  mihomo %s %s · панель %s\n", c.bold("VPN"), s.Mihomo.Version, c.green("работает"), s.Version)
	} else {
		fmt.Printf("%s  mihomo %s: %s\n", c.bold("VPN"), c.red("не отвечает"), s.Mihomo.Error)
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
		via = "нет включённых серверов — напрямую"
	}
	if s.Mode == "DIRECT" {
		via = "напрямую, без VPN"
	}
	fmt.Printf("Трафик через: %s  %s  %s\n", c.bold(firstNonEmpty(via, "—")), c.delayColored(delay), c.dim("режим: "+firstNonEmpty(modeRU[s.Mode], s.Mode)))
	if a != nil && a.OK {
		fmt.Printf("Сейчас: ↓ %s  ↑ %s  соединений %d  · всего ↓ %s ↑ %s\n",
			c.cyan(rateRU(a.DownRate)), c.cyan(rateRU(a.UpRate)), a.Conns, bytesRU(a.DnTotal), bytesRU(a.UpTotal))
	}
}

func (c *cli) printServers(s *cliState) {
	if len(s.Servers) == 0 {
		fmt.Println(c.dim("  Серверов в панели нет (серверы из env SRV/SUB здесь не показываются)."))
		return
	}
	fmt.Println(c.dim(" #  " + pad("", 2) + pad("Имя", 20) + " " + pad("Тип", 7) + " " + pad("Адрес", 30) + " " + padL("Пинг", 10)))
	for i, x := range s.Servers {
		mark := c.dim("○")
		switch {
		case !x.Enabled:
			mark = c.dim("·")
		case !x.Loaded:
			mark = c.red("✕")
		case x.Delay == 0:
			mark = c.red("●")
		case x.Delay > 0:
			mark = c.green("●")
		default:
			mark = c.yellow("●")
		}
		name := pad(x.Name, 20)
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
			ping = c.dim(padL("выкл", 10))
		}
		note := ""
		if x.Active {
			note = c.cyan(" ← сейчас")
		}
		if x.Type == "naive" && x.Naive != nil && x.Naive.State != "running" && x.Enabled {
			note += c.yellow(" naive: " + x.Naive.State)
		}
		if x.Enabled && !x.Loaded && s.Mihomo.OK {
			note += c.red(" ссылка не принята")
		}
		fmt.Printf("%2d  %s %s %s %s %s%s\n", i+1, mark, name, pad(firstNonEmpty(typeShort[x.Type], x.Type), 7), pad(addr, 30), ping, note)
	}
}

var typeShort = map[string]string{"vless": "VLESS", "hysteria2": "HY2", "mieru": "Mieru", "naive": "Naive", "vmess": "VMess",
	"trojan": "Trojan", "ss": "SS", "tuic": "TUIC", "anytls": "AnyTLS", "hysteria": "HY1", "socks5": "SOCKS5"}

func (c *cli) printRecent(a *Activity, n int) {
	if len(a.Recent) == 0 {
		fmt.Println(c.dim("  новых соединений пока нет"))
		return
	}
	for i, r := range a.Recent {
		if i >= n {
			break
		}
		fmt.Println("  " + c.connLine(r))
	}
}

func (c *cli) connLine(r ConnView) string {
	dst := firstNonEmpty(r.Host, r.DstIP)
	if r.Port != "" {
		dst += ":" + r.Port
	}
	via := r.Via
	switch {
	case r.Internal:
		via = c.dim("служебное")
	case r.Rejected:
		via = c.red(via)
	case r.Direct:
		via = c.yellow(via)
	default:
		via = c.green(via)
	}
	return fmt.Sprintf("%s %-3s %-15s → %s %s %s", c.dim(r.Start.Local().Format("15:04:05")), r.Net, r.Src, pad(dst, 40), c.dim("через"), via)
}

func (c *cli) cmdStatus() error {
	s, err := c.state()
	if err != nil {
		return err
	}
	a, _ := c.activity(10)
	c.printHeader(s, a)
	if a != nil && len(a.Events) > 0 {
		fmt.Println(c.dim("События:"))
		for i, e := range a.Events {
			if i >= 5 {
				break
			}
			fmt.Printf("  %s %s\n", c.dim(e.T.Local().Format("15:04:05")), e.Msg)
		}
	}
	if a != nil {
		fmt.Println(c.dim("Последние запросы:"))
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
			return nil, fmt.Errorf("нет сервера №%d", n)
		}
		return &s.Servers[n-1], nil
	}
	for i := range s.Servers {
		if strings.EqualFold(s.Servers[i].Name, arg) {
			return &s.Servers[i], nil
		}
	}
	return nil, fmt.Errorf("сервер «%s» не найден", arg)
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
	fmt.Println(c.green("✓ ") + "трафик идёт через «" + firstNonEmpty(r.Proxy, x.Name) + "» (режим «вручную»)")
	return nil
}

func (c *cli) cmdMode(arg string) error {
	m, ok := modeArg(arg)
	if !ok {
		return fmt.Errorf("неизвестный режим «%s»", arg)
	}
	if err := c.call(http.MethodPost, "/api/mode", map[string]string{"mode": m}, nil); err != nil {
		return err
	}
	fmt.Println(c.green("✓ ") + "режим: " + modeRU[m])
	return nil
}

func (c *cli) cmdTest(arg string) error {
	if strings.TrimSpace(arg) == "" {
		fmt.Println("Проверяю все серверы…")
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
	fmt.Printf("Проверяю «%s»…\n", x.Name)
	if err := c.call(http.MethodPost, "/api/servers/"+x.ID+"/test", nil, &r); err != nil {
		return err
	}
	fmt.Printf("«%s»: %s\n", x.Name, c.delayColored(r.Delay))
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
	shown := 0
	for _, x := range r.Connections {
		byVia[x.Via]++
		if f != "" && !strings.Contains(strings.ToLower(x.Host+" "+x.DstIP+" "+x.Src+" "+x.Via), f) {
			continue
		}
		if shown < limit {
			fmt.Printf("  %s  %s\n", c.connLine(x), c.dim("↓"+bytesRU(x.Down)+" ↑"+bytesRU(x.Up)))
		}
		shown++
	}
	parts := []string{}
	for k, n := range byVia {
		parts = append(parts, fmt.Sprintf("%s: %d", k, n))
	}
	sort.Strings(parts)
	fmt.Printf("Всего соединений %d", len(r.Connections))
	if len(parts) > 0 {
		fmt.Printf(" (%s)", strings.Join(parts, ", "))
	}
	if f != "" {
		fmt.Printf(", по фильтру «%s»: %d", filter, shown)
	}
	if shown > limit {
		fmt.Printf(", показаны первые %d", limit)
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
			fmt.Println(c.dim("по фильтру «" + q + "» ничего нет"))
		} else {
			fmt.Println(c.dim("журнал пуст"))
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
	fmt.Println(c.dim("— новые строки; Enter — выход —"))
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
		if len(a.Events) > 0 {
			fmt.Println(c.dim("События:"))
			for i, e := range a.Events {
				if i >= 3 {
					break
				}
				fmt.Printf("  %s %s\n", c.dim(e.T.Local().Format("15:04:05")), e.Msg)
			}
		}
		fmt.Println(c.dim("Последние запросы:"))
		c.printRecent(a, 15)
		fmt.Println(c.dim("— обновление каждые 2 с; Enter — назад —"))
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
	bars := []rune("▁▂▃▄▅▆▇█")
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
	return c.dim("↓ за ") + c.dim(strconv.Itoa(len(h)*5/60)+" мин: ") + c.cyan(b.String()) + c.dim(" пик "+rateRU(mx))
}

func (c *cli) add(text string) error {
	var r struct {
		Added   int      `json:"added"`
		Errors  []string `json:"errors"`
		Warning string   `json:"warning"`
	}
	err := c.call(http.MethodPost, "/api/servers", map[string]string{"text": text}, &r)
	if r.Added > 0 {
		fmt.Printf("%s добавлено: %d\n", c.green("✓"), r.Added)
	}
	for _, e := range r.Errors {
		fmt.Println(c.red("  ✕ ") + e)
	}
	if r.Warning != "" {
		fmt.Println(c.yellow("  mihomo не перечитал список: ") + r.Warning)
	}
	if err != nil && r.Added == 0 && len(r.Errors) == 0 {
		return err
	}
	return nil
}

// ---------- интерактивное меню ----------

func (c *cli) help() {
	fmt.Print(`Команды (N — номер сервера из списка):
  Enter / r   обновить              w         монитор: скорость и запросы вживую
  u N         использовать N        s [текст] текущие соединения (фильтр)
  c N         проверить N           j [текст] журнал mihomo (фильтр)
  t           проверить все         f [текст] журнал вживую
  m           режим (авто/резерв/вручную/балансировка/без VPN)
  a           добавить (вставить ссылки)
  e N         изменить N            o N       включить/выключить N
  k N         поднять N выше        d N       удалить N
  l N         журнал naive N        b         бэкап (JSON на экран)
  q           выход
Разовые команды: vpn status | list | use N | mode auto | test [N] | conns [текст] | logs [-f] [текст]
Терминал RouterOS не показывает кириллицу — по умолчанию вывод транслитом.
В нормальном SSH-терминале: vpn --utf8 (или VPN_UTF8=1, LANG=*.UTF-8).
`)
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
		fmt.Println(c.dim("u N использовать · c N проверить · t все · m режим · a добавить · e/o/d/k N · w монитор · s соединения · j журнал · ? помощь · q выход"))
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
			fmt.Println("Проверяю все…")
			cerr = c.call(http.MethodPost, "/api/test-all", nil, nil)
			if cerr == nil {
				msg = c.green("✓ проверено")
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
				if l, _ := c.readLine(fmt.Sprintf("Удалить «%s»? (да/нет) ", x.Name)); !isYes(l) {
					return nil
				}
				return c.call(http.MethodDelete, "/api/servers/"+x.ID, nil, nil)
			})
		case "k":
			cerr = c.withArg(arg, func(a string) error { return c.moveUp(s, a) })
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
				fmt.Println(c.bold("Журнал naive: " + x.Name))
				if len(r.Lines) == 0 {
					fmt.Println(c.dim("пусто — процесс не запускался (это не naive-сервер?)"))
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
				fmt.Println(c.yellow("В бэкапе пароли серверов. Скопируйте и сохраните в надёжном месте."))
				c.pause()
			}
		default:
			msg = c.yellow("не понял «" + line + "». ? — помощь")
		}
		if cerr != nil {
			msg = c.red("ошибка: ") + cerr.Error()
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

func (c *cli) pause() { c.readLine(c.dim("Enter — назад ")) }

func (c *cli) withArg(arg string, fn func(string) error) error {
	if arg == "" {
		l, ok := c.readLine("Номер сервера: ")
		if !ok || l == "" {
			return nil
		}
		arg = l
	}
	return fn(arg)
}

func (c *cli) menuMode(cur string) error {
	fmt.Println("Режим:")
	for i, k := range []string{"AUTO", "FALLBACK", "MANUAL", "LOADBALANCE", "DIRECT"} {
		mark := "  "
		if k == cur {
			mark = c.cyan("→ ")
		}
		fmt.Printf("  %s%d  %s\n", mark, i+1, map[string]string{"AUTO": "авто (самый быстрый)", "FALLBACK": "резерв по порядку",
			"MANUAL": "вручную", "LOADBALANCE": "балансировка", "DIRECT": "без VPN"}[k])
	}
	l, ok := c.readLine("Номер (пусто — отмена): ")
	if !ok || l == "" {
		return nil
	}
	return c.cmdMode(l)
}

func (c *cli) menuAdd() error {
	fmt.Println("Вставьте ссылки по одной в строке (vless://, hy2://, mierus://, naive+https://…).")
	fmt.Println(c.dim("Пустая строка — закончить."))
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
	fmt.Printf("Имя: %s\n", full.Name)
	name, ok := c.readLine("Новое имя (пусто — оставить): ")
	if !ok {
		return nil
	}
	fmt.Printf("Ссылка: %s\n", c.dim(full.URI))
	uri, ok := c.readLine("Новая ссылка (пусто — оставить): ")
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

func (c *cli) moveUp(s *cliState, arg string) error {
	n, err := strconv.Atoi(arg)
	if err != nil || n < 2 || n > len(s.Servers) {
		return fmt.Errorf("укажите номер 2…%d", len(s.Servers))
	}
	ids := make([]string, len(s.Servers))
	for i, x := range s.Servers {
		ids[i] = x.ID
	}
	ids[n-2], ids[n-1] = ids[n-1], ids[n-2]
	return c.call(http.MethodPost, "/api/reorder", map[string]any{"ids": ids}, nil)
}
