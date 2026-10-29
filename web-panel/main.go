// mihomo-web — веб-управление серверами для образа mihomo-mikrotik.
//
//	mihomo-web render  — сгенерировать servers.txt из servers.json (вызывается entrypoint до старта mihomo)
//	mihomo-web serve   — веб-панель + супервизор naive
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed static
var staticFS embed.FS

var version = "dev"

type Config struct {
	WorkDir   string
	DataDir   string
	Listen    string
	User      string
	Password  string
	MihomoAPI string
	Secret    string
	Provider  string
	TestURL   string
	NaiveBin  string
	NaiveUID  int
	NaiveBase int
	NaiveDNS  string
	GroupSel  string
	GroupMan  string
	TimeoutMs int
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(env(k, "")); err == nil {
		return v
	}
	return def
}

func loadConfig() Config {
	wd := env("WORKDIR", "/etc/mihomo")
	return Config{
		WorkDir:   wd,
		DataDir:   env("WEB_DATA_DIR", filepath.Join(wd, "web")),
		Listen:    env("WEB_LISTEN", "0.0.0.0") + ":" + env("WEB_PORT", "8080"),
		User:      env("WEB_USER", "admin"),
		Password:  env("WEB_PASSWORD", os.Getenv("UI_SECRET")),
		MihomoAPI: "http://127.0.0.1:" + env("UI_PORT", "9090"),
		Secret:    os.Getenv("UI_SECRET"),
		Provider:  env("WEB_PROVIDER", "WEB"),
		TestURL:   env("HEALTH_CHECK_URL", "https://www.gstatic.com/generate_204"),
		NaiveBin:  env("NAIVE_BIN", "/usr/bin/naive"),
		NaiveUID:  envInt("NAIVE_UID", 2080),
		NaiveBase: envInt("NAIVE_BASE_PORT", 20800),
		NaiveDNS:  env("NAIVE_DNS", ""),
		GroupSel:  env("WEB_GROUP_SELECTOR", "SELECTOR"),
		GroupMan:  env("WEB_GROUP_MANUAL", "MANUAL"),
		TimeoutMs: envInt("HEALTH_CHECK_TIMEOUT", 5000),
	}
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).With("app", "mihomo-web")
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg := loadConfig()
	store, err := NewStore(cfg.DataDir, cfg.NaiveBase)
	if err != nil {
		logger.Error("store init failed", "err", err)
		os.Exit(1)
	}
	switch cmd {
	case "render":
		if err := store.Render(); err != nil {
			logger.Error("render failed", "err", err)
			os.Exit(1)
		}
		logger.Info("servers.txt rendered", "servers", len(store.List()))
	case "serve":
		os.Exit(serve(cfg, store, logger))
	case "version", "-v", "--version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "usage: %s [serve|render|version]\n", os.Args[0])
		os.Exit(2)
	}
}

type App struct {
	cfg    Config
	store  *Store
	mh     *Mihomo
	naive  *NaiveManager
	log    *slog.Logger
	applyM sync.Mutex
}

func serve(cfg Config, store *Store, logger *slog.Logger) int {
	app := &App{
		cfg:   cfg,
		store: store,
		mh:    NewMihomo(cfg.MihomoAPI, cfg.Secret, cfg.Provider, cfg.TestURL),
		naive: NewNaiveManager(cfg.NaiveBin, cfg.NaiveUID, cfg.NaiveDNS, logger),
		log:   logger,
	}
	if err := store.Render(); err != nil {
		logger.Error("render failed", "err", err)
	}
	app.naive.Sync(store.List())

	if cfg.Password == "" {
		logger.Warn("WEB_PASSWORD и UI_SECRET не заданы — панель открыта без пароля")
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Listen, "version", version, "naive", app.naive.Available())
		errCh <- srv.ListenAndServe()
	}()
	// первичная синхронизация с mihomo, когда тот поднимется
	go func() {
		for i := 0; i < 60; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, err := app.mh.Version(ctx)
			cancel()
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := app.mh.ReloadProvider(ctx); err != nil {
					logger.Warn("initial provider reload", "err", err)
				}
				cancel()
				return
			}
			time.Sleep(2 * time.Second)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case s := <-sig:
		logger.Info("shutdown", "signal", s.String())
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server", "err", err)
			app.naive.StopAll()
			return 1
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	app.naive.StopAll()
	return 0
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/state", a.hState)
	mux.HandleFunc("POST /api/servers", a.hAdd)
	mux.HandleFunc("PUT /api/servers/{id}", a.hUpdate)
	mux.HandleFunc("DELETE /api/servers/{id}", a.hDelete)
	mux.HandleFunc("POST /api/servers/{id}/test", a.hTest)
	mux.HandleFunc("POST /api/servers/{id}/use", a.hUse)
	mux.HandleFunc("GET /api/servers/{id}/log", a.hLog)
	mux.HandleFunc("POST /api/test-all", a.hTestAll)
	mux.HandleFunc("POST /api/mode", a.hMode)
	mux.HandleFunc("POST /api/reorder", a.hReorder)
	mux.HandleFunc("GET /api/export", a.hExport)
	mux.HandleFunc("POST /api/import", a.hImport)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return a.middleware(mux)
}

func (a *App) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.URL.Path != "/healthz" && a.cfg.Password != "" {
			u, p, ok := r.BasicAuth()
			if !ok ||
				subtle.ConstantTimeCompare([]byte(u), []byte(a.cfg.User)) != 1 ||
				subtle.ConstantTimeCompare([]byte(p), []byte(a.cfg.Password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="mihomo-web", charset="UTF-8"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		// защита от CSRF: изменяющие запросы только из нашего JS
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Mihomo-Web") != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet {
			a.log.Info("api", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr, "dur", time.Since(start).Round(time.Millisecond))
		}
	})
}

// apply: servers.json -> servers.txt -> naive -> reload провайдера в mihomo.
func (a *App) apply(ctx context.Context) string {
	a.applyM.Lock()
	defer a.applyM.Unlock()
	list := a.store.List()
	a.naive.Sync(list)
	if hasNaive(list) {
		time.Sleep(700 * time.Millisecond) // дать naive открыть порт до health-check
	}
	if err := a.mh.ReloadProvider(ctx); err != nil {
		a.log.Warn("provider reload failed", "err", err)
		return err.Error()
	}
	// после reload у прокси пустая история задержек — обновляем в фоне,
	// заодно url-test/fallback пересчитают выбор
	go func() {
		hctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := a.mh.HealthCheckAll(hctx); err != nil {
			a.log.Debug("background healthcheck", "err", err)
		}
	}()
	return ""
}

func hasNaive(l []Server) bool {
	for _, s := range l {
		if s.Type == "naive" && s.Enabled {
			return true
		}
	}
	return false
}

type serverView struct {
	Server
	Host    string       `json:"host"`
	Port    string       `json:"port"`
	Proxies []string     `json:"proxies"`
	Loaded  bool         `json:"loaded"`
	Alive   bool         `json:"alive"`
	Delay   int          `json:"delay"`
	Active  bool         `json:"active"`
	Naive   *NaiveStatus `json:"naive,omitempty"`
}

func (a *App) hState(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	out := map[string]any{"version": version, "naive_available": a.naive.Available(), "placeholder": EmptyPlaceholder}

	mver, merr := a.mh.Version(ctx)
	out["mihomo"] = map[string]any{"ok": merr == nil, "version": mver, "error": errStr(merr)}

	var loaded map[string]ProxyInfo
	var mode, manual, autoNow string
	if merr == nil {
		loaded, _ = a.mh.ProviderProxies(ctx)
		if g, err := a.mh.Group(ctx, a.cfg.GroupSel); err == nil {
			mode = g.Now
		}
		if g, err := a.mh.Group(ctx, a.cfg.GroupMan); err == nil {
			manual = g.Now
		}
		if mode != "" && mode != a.cfg.GroupMan && mode != "DIRECT" {
			if g, err := a.mh.Group(ctx, mode); err == nil {
				autoNow = g.Now
			}
		}
	}
	out["mode"], out["manual"], out["current"] = mode, manual, autoNow

	current := autoNow
	if mode == a.cfg.GroupMan {
		current = manual
	}
	views := []serverView{}
	for _, s := range a.store.List() {
		v := serverView{Server: s, Proxies: ProxyNames(&s), Delay: -1}
		v.URI = "" // ссылки с секретами отдаём только в редактировании/экспорте
		if p, err := ParseURI(s.URI); err == nil {
			v.Host, v.Port = p.Host, p.Port
		}
		for _, pn := range v.Proxies {
			if pi, ok := loaded[pn]; ok {
				v.Loaded = true
				v.Alive = v.Alive || pi.Alive
				if d := pi.LastDelay(); d > 0 && (v.Delay <= 0 || d < v.Delay) {
					v.Delay = d
				} else if v.Delay < 0 {
					v.Delay = d
				}
			}
			if pn == current {
				v.Active = true
			}
		}
		if s.Type == "naive" {
			if st, ok := a.naive.Status(s.ID); ok {
				v.Naive = &st
			}
		}
		views = append(views, v)
	}
	out["servers"] = views
	writeJSON(w, http.StatusOK, out)
}

func (a *App) hAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var added []Server
	var errs []string
	lines := strings.Split(strings.ReplaceAll(req.Text, "\r", ""), "\n")
	nonEmpty := 0
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, "//") {
			continue
		}
		nonEmpty++
		name := ""
		if nonEmpty == 1 {
			name = req.Name
		}
		s, err := a.store.Add(ln, name)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", short(ln), err))
			continue
		}
		added = append(added, s)
	}
	if nonEmpty == 0 {
		writeErr(w, http.StatusBadRequest, "вставьте хотя бы одну ссылку")
		return
	}
	warn := ""
	if len(added) > 0 {
		warn = a.apply(r.Context())
	}
	code := http.StatusOK
	if len(added) == 0 {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, map[string]any{"added": len(added), "errors": errs, "warning": warn})
}

func (a *App) hUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var p Patch
	if !readJSON(w, r, &p) {
		return
	}
	if p.URI != nil && strings.TrimSpace(*p.URI) == "" {
		p.URI = nil // пустое поле = ссылку не менять
	}
	s, err := a.store.Update(id, p)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": s.ID, "warning": a.apply(r.Context())})
}

func (a *App) hDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Delete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"warning": a.apply(r.Context())})
}

func (a *App) hTest(w http.ResponseWriter, r *http.Request) {
	s, ok := a.store.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, errNotFound.Error())
		return
	}
	res := map[string]int{}
	best := 0
	for _, pn := range ProxyNames(&s) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(a.cfg.TimeoutMs+3000)*time.Millisecond)
		d, err := a.mh.Delay(ctx, pn, a.cfg.TimeoutMs)
		cancel()
		if err != nil {
			d = 0
		}
		res[pn] = d
		if d > 0 && (best == 0 || d < best) {
			best = d
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"delay": best, "detail": res})
}

func (a *App) hTestAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := a.mh.HealthCheckAll(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) hUse(w http.ResponseWriter, r *http.Request) {
	s, ok := a.store.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, errNotFound.Error())
		return
	}
	if !s.Enabled {
		writeErr(w, http.StatusBadRequest, "сервер выключен — сначала включите его")
		return
	}
	names := ProxyNames(&s)
	if len(names) == 0 {
		writeErr(w, http.StatusBadRequest, "у сервера нет прокси")
		return
	}
	name := names[0]
	if q := r.URL.Query().Get("proxy"); q != "" {
		for _, n := range names {
			if n == q {
				name = q
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := a.mh.Select(ctx, a.cfg.GroupMan, name); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := a.mh.Select(ctx, a.cfg.GroupSel, a.cfg.GroupMan); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	_ = a.mh.CloseConnections(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proxy": name})
}

var allowedModes = map[string]bool{"AUTO": true, "FALLBACK": true, "MANUAL": true, "LOADBALANCE": true, "DIRECT": true}

func (a *App) hMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !allowedModes[req.Mode] {
		writeErr(w, http.StatusBadRequest, "неизвестный режим")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := a.mh.Select(ctx, a.cfg.GroupSel, req.Mode); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	_ = a.mh.CloseConnections(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) hReorder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	for i, id := range req.IDs {
		o := i
		if _, err := a.store.Update(id, Patch{Order: &o}); err != nil && !errors.Is(err, errNotFound) {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"warning": a.apply(r.Context())})
}

func (a *App) hLog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"lines": a.naive.Logs(r.PathValue("id"))})
}

func (a *App) hExport(w http.ResponseWriter, r *http.Request) {
	if id := r.URL.Query().Get("id"); id != "" {
		s, ok := a.store.Get(id)
		if !ok {
			writeErr(w, http.StatusNotFound, errNotFound.Error())
			return
		}
		writeJSON(w, http.StatusOK, s)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="mihomo-servers-%s.json"`, time.Now().Format("20060102-1504")))
	writeJSON(w, http.StatusOK, map[string]any{"version": 1, "servers": a.store.List()})
}

func (a *App) hImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Servers []Server `json:"servers"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Servers) == 0 {
		writeErr(w, http.StatusBadRequest, "в файле нет серверов")
		return
	}
	n, errs, err := a.store.Replace(req.Servers)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": n, "errors": errs, "warning": a.apply(r.Context())})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "неверный JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func short(s string) string {
	if i := strings.Index(s, "://"); i > 0 && len(s) > i+3+12 {
		return s[:i+3+12] + "…"
	}
	return s
}
