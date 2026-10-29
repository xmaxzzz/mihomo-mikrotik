package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	URI       string    `json:"uri"`
	Enabled   bool      `json:"enabled"`
	LocalPort int       `json:"local_port,omitempty"` // только naive
	Order     int       `json:"order"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type storeData struct {
	Version int      `json:"version"`
	Servers []Server `json:"servers"`
}

type Store struct {
	mu       sync.Mutex
	dataPath string // servers.json — источник правды
	outPath  string // servers.txt — file-provider для mihomo
	basePort int
	data     storeData
}

func NewStore(dir string, basePort int) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		dataPath: filepath.Join(dir, "servers.json"),
		outPath:  filepath.Join(dir, "servers.txt"),
		basePort: basePort,
		data:     storeData{Version: 1},
	}
	buf, err := os.ReadFile(s.dataPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(buf, &s.data); err != nil {
		// битый файл не затираем молча — откладываем копию
		bak := s.dataPath + ".broken-" + time.Now().Format("20060102-150405")
		_ = os.WriteFile(bak, buf, 0o600)
		return nil, fmt.Errorf("servers.json повреждён (копия: %s): %w", bak, err)
	}
	return s, nil
}

func (s *Store) List() []Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Server, len(s.data.Servers))
	copy(out, s.data.Servers)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

func (s *Store) Get(id string) (Server, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sv := range s.data.Servers {
		if sv.ID == id {
			return sv, true
		}
	}
	return Server{}, false
}

// Add добавляет сервер из ссылки. name пустой — берётся из ссылки или host:port.
func (s *Store) Add(uri, name string) (Server, error) {
	p, err := ParseURI(uri)
	if err != nil {
		return Server{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	name = cleanName(firstNonEmpty(name, p.Name, hostPort(p)))
	sv := Server{
		ID:        newID(),
		Name:      s.uniqueName(name, ""),
		Type:      p.Type,
		URI:       strings.TrimSpace(uri),
		Enabled:   true,
		Order:     s.nextOrder(),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if sv.Type == "naive" {
		sv.LocalPort = s.freePort("")
	}
	s.data.Servers = append(s.data.Servers, sv)
	return sv, s.saveLocked()
}

type Patch struct {
	Name    *string `json:"name"`
	URI     *string `json:"uri"`
	Enabled *bool   `json:"enabled"`
	Order   *int    `json:"order"`
}

func (s *Store) Update(id string, p Patch) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i := range s.data.Servers {
		if s.data.Servers[i].ID == id {
			idx = i
		}
	}
	if idx < 0 {
		return Server{}, errNotFound
	}
	sv := s.data.Servers[idx]
	if p.URI != nil {
		pu, err := ParseURI(*p.URI)
		if err != nil {
			return Server{}, err
		}
		sv.URI = strings.TrimSpace(*p.URI)
		sv.Type = pu.Type
		if sv.Type == "naive" && sv.LocalPort == 0 {
			sv.LocalPort = s.freePort(sv.ID)
		}
		if sv.Type != "naive" {
			sv.LocalPort = 0
		}
	}
	if p.Name != nil {
		n := cleanName(*p.Name)
		if n == "" {
			return Server{}, errors.New("имя не может быть пустым")
		}
		if s.nameTaken(n, sv.ID) {
			return Server{}, fmt.Errorf("имя %q уже занято", n)
		}
		sv.Name = n
	}
	if p.Enabled != nil {
		sv.Enabled = *p.Enabled
	}
	if p.Order != nil {
		sv.Order = *p.Order
	}
	sv.UpdatedAt = time.Now().UTC()
	s.data.Servers[idx] = sv
	return sv, s.saveLocked()
}

var errNotFound = errors.New("сервер не найден")

// EmptyPlaceholder — имя direct-заглушки, когда нет ни одного включённого сервера.
const EmptyPlaceholder = "Нет серверов (напрямую)"

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Servers {
		if s.data.Servers[i].ID == id {
			s.data.Servers = append(s.data.Servers[:i], s.data.Servers[i+1:]...)
			return s.saveLocked()
		}
	}
	return errNotFound
}

// Replace — восстановление из бэкапа. Ссылки валидируются, битые отбрасываются.
func (s *Store) Replace(in []Server) (int, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []string
	s.data.Servers = nil
	for i, sv := range in {
		p, err := ParseURI(sv.URI)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%q: %v", sv.Name, err))
			continue
		}
		sv.Type = p.Type
		if sv.ID == "" {
			sv.ID = newID()
		}
		sv.Name = s.uniqueName(cleanName(firstNonEmpty(sv.Name, p.Name, hostPort(p))), "")
		sv.Order = i
		sv.LocalPort = 0
		if sv.CreatedAt.IsZero() {
			sv.CreatedAt = time.Now().UTC()
		}
		sv.UpdatedAt = time.Now().UTC()
		s.data.Servers = append(s.data.Servers, sv)
	}
	for i := range s.data.Servers {
		if s.data.Servers[i].Type == "naive" {
			s.data.Servers[i].LocalPort = s.freePort(s.data.Servers[i].ID)
		}
	}
	return len(s.data.Servers), errs, s.saveLocked()
}

// Render пишет servers.txt для file-провайдера mihomo.
// Ссылки кодируются base64: mihomo сначала пробует YAML, и сырые строки
// со спецсимволами могли бы случайно распарситься как YAML-map.
func (s *Store) Render() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renderLocked()
}

func (s *Store) renderLocked() error {
	var lines []string
	srv := make([]Server, len(s.data.Servers))
	copy(srv, s.data.Servers)
	sort.SliceStable(srv, func(i, j int) bool { return srv[i].Order < srv[j].Order })
	for _, sv := range srv {
		if !sv.Enabled {
			continue
		}
		if sv.Type == "naive" {
			if sv.LocalPort == 0 {
				continue
			}
			lines = append(lines, WithName("socks5://127.0.0.1:"+strconv.Itoa(sv.LocalPort), sv.Name))
			continue
		}
		lines = append(lines, WithName(sv.URI, sv.Name))
	}
	var content []byte
	if len(lines) == 0 {
		// mihomo не принимает пустой провайдер и при ошибке оставляет СТАРЫЙ список,
		// поэтому при нуле серверов кладём direct-заглушку (как в исходном образе: нет серверов = напрямую)
		content = []byte("proxies:\n  - name: \"" + EmptyPlaceholder + "\"\n    type: direct\n    udp: true\n")
	} else {
		content = []byte(base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\n"))))
	}
	return atomicWrite(s.outPath, content, 0o600)
}

func (s *Store) saveLocked() error {
	buf, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(s.dataPath, buf, 0o600); err != nil {
		return err
	}
	return s.renderLocked()
}

func (s *Store) nextOrder() int {
	m := -1
	for _, sv := range s.data.Servers {
		if sv.Order > m {
			m = sv.Order
		}
	}
	return m + 1
}

func (s *Store) nameTaken(name, exceptID string) bool {
	for _, sv := range s.data.Servers {
		if sv.ID != exceptID && strings.EqualFold(sv.Name, name) {
			return true
		}
	}
	return false
}

func (s *Store) uniqueName(name, exceptID string) string {
	if name == "" {
		name = "server"
	}
	if !s.nameTaken(name, exceptID) {
		return name
	}
	for i := 2; ; i++ {
		n := fmt.Sprintf("%s %d", name, i)
		if !s.nameTaken(n, exceptID) {
			return n
		}
	}
}

func (s *Store) freePort(exceptID string) int {
	used := map[int]bool{}
	for _, sv := range s.data.Servers {
		if sv.ID != exceptID && sv.LocalPort > 0 {
			used[sv.LocalPort] = true
		}
	}
	for p := s.basePort; p < s.basePort+1000; p++ {
		if !used[p] {
			return p
		}
	}
	return 0
}

// cleanName убирает символы, ломающие имена в mihomo/группах.
func cleanName(n string) string {
	n = strings.TrimSpace(n)
	n = strings.Map(func(r rune) rune {
		if r < 32 || r == '\u007f' {
			return -1
		}
		return r
	}, n)
	if len([]rune(n)) > 64 {
		n = string([]rune(n)[:64])
	}
	return n
}

func hostPort(p *ParsedURI) string {
	if p.Port == "" {
		return p.Host
	}
	return p.Host + ":" + p.Port
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
