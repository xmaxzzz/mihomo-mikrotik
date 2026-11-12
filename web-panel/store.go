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
	Sub       *SubInfo  `json:"sub,omitempty"`    // только у Type == "sub": ссылка подписки в URI
	Parent    string    `json:"parent,omitempty"` // сервер из подписки: ID подписки
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
	for _, sv := range s.everyLocked() {
		if sv.ID == id {
			return sv, true
		}
	}
	return Server{}, false
}

// everyLocked — все серверы: верхнего уровня и из подписок (копии, Parent заполнен).
func (s *Store) everyLocked() []Server {
	out := make([]Server, 0, len(s.data.Servers))
	for _, sv := range s.data.Servers {
		out = append(out, sv)
		if sv.Type == "sub" && sv.Sub != nil {
			for _, it := range sv.Sub.Items {
				it.Parent = sv.ID
				it.Enabled = sv.Enabled
				out = append(out, it)
			}
		}
	}
	return out
}

// Expanded — список для отображения и работы: подписка, сразу за ней её серверы.
// Серверы подписки включены/выключены вместе с ней.
func (s *Store) Expanded() []Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expandedLocked()
}

func (s *Store) expandedLocked() []Server {
	top := make([]Server, len(s.data.Servers))
	copy(top, s.data.Servers)
	sort.SliceStable(top, func(i, j int) bool { return top[i].Order < top[j].Order })
	out := make([]Server, 0, len(top))
	for _, sv := range top {
		out = append(out, sv)
		if sv.Type == "sub" && sv.Sub != nil {
			for _, it := range sv.Sub.Items {
				it.Parent = sv.ID
				it.Enabled = sv.Enabled
				out = append(out, it)
			}
		}
	}
	return out
}

// Active — включённые реальные серверы (без строк-подписок) в порядке списка: для naive и mihomo.
func (s *Store) Active() []Server {
	var out []Server
	for _, sv := range s.Expanded() {
		if sv.Enabled && sv.Type != "sub" {
			out = append(out, sv)
		}
	}
	return out
}

// Add добавляет сервер из ссылки. name пустой — берётся из ссылки или host:port.
func (s *Store) Add(uri, name string) (Server, error) {
	if isSubURL(uri) {
		return s.addSub(uri, name)
	}
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

func (s *Store) addSub(raw, name string) (Server, error) {
	u, err := validateSubURL(raw)
	if err != nil {
		return Server{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sv := range s.data.Servers {
		if sv.Type == "sub" && sv.URI == u {
			return Server{}, Ef("подписка уже добавлена: %s", "subscription already added: %s", sv.Name)
		}
	}
	sv := Server{
		ID:        newID(),
		Name:      s.uniqueName(cleanName(firstNonEmpty(name, subHost(u))), ""),
		Type:      "sub",
		URI:       u,
		Enabled:   true,
		Order:     s.nextOrder(),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Sub:       &SubInfo{},
	}
	s.data.Servers = append(s.data.Servers, sv)
	return sv, s.saveLocked()
}

// SetSubResult сохраняет итог обновления подписки. Серверы с той же ссылкой сохраняют
// ID, имя и локальный порт naive (чтобы не дёргать процессы и выбор в mihomo).
// Возвращает true, если набор серверов изменился.
func (s *Store) SetSubResult(id string, res *subResult, ferr error) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i := range s.data.Servers {
		if s.data.Servers[i].ID == id && s.data.Servers[i].Type == "sub" {
			idx = i
		}
	}
	if idx < 0 {
		return false, errNotFound
	}
	sv := &s.data.Servers[idx]
	if sv.Sub == nil {
		sv.Sub = &SubInfo{}
	}
	now := time.Now().UTC()
	sv.Sub.CheckedAt = now
	if ferr != nil {
		sv.Sub.Error, sv.Sub.ErrorEN = errText(ferr, false), errText(ferr, true)
		return false, s.saveLocked()
	}
	old := map[string]Server{}
	for _, it := range sv.Sub.Items {
		old[it.URI] = it
	}
	before := len(sv.Sub.Items)
	sv.Sub.Items = nil // имена и порты считаем заново, без старого набора этой подписки
	changed := false
	for _, it := range res.Items {
		var ch Server
		if o, ok := old[it.URI]; ok {
			ch = o
			ch.Name = s.uniqueName(o.Name, o.ID)
			delete(old, it.URI)
		} else {
			changed = true
			ch = Server{ID: newID(), Name: s.uniqueName(firstNonEmpty(it.Name, "server"), ""), Type: it.Type,
				URI: it.URI, Enabled: true, CreatedAt: now}
		}
		ch.Type, ch.Parent, ch.Order, ch.UpdatedAt = it.Type, "", len(sv.Sub.Items), now
		if ch.Type == "naive" {
			if ch.LocalPort == 0 || s.portTaken(ch.LocalPort, ch.ID) {
				ch.LocalPort = s.freePort(ch.ID)
			}
		} else {
			ch.LocalPort = 0
		}
		sv.Sub.Items = append(sv.Sub.Items, ch)
	}
	if len(old) > 0 || before != len(sv.Sub.Items) {
		changed = true
	}
	sv.Sub.UpdatedAt, sv.Sub.Error, sv.Sub.ErrorEN = now, "", ""
	sv.Sub.Skipped = res.Skipped
	sv.Sub.IntervalH = res.IntervalH
	sv.Sub.Upload, sv.Sub.Download, sv.Sub.Total, sv.Sub.Expire = res.Upload, res.Download, res.Total, res.Expire
	return changed, s.saveLocked()
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
		if s.isChildLocked(id) {
			return Server{}, errChild
		}
		return Server{}, errNotFound
	}
	sv := s.data.Servers[idx]
	if p.URI != nil && sv.Type == "sub" {
		u, err := validateSubURL(*p.URI)
		if err != nil {
			return Server{}, err
		}
		sv.URI = u
	} else if p.URI != nil {
		if isSubURL(*p.URI) {
			return Server{}, E("сервер нельзя превратить в подписку — добавьте подписку отдельно",
				"a server cannot become a subscription - add the subscription separately")
		}
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
			return Server{}, E("имя не может быть пустым", "name must not be empty")
		}
		if s.nameTaken(n, sv.ID) {
			return Server{}, Ef("имя %q уже занято", "name %q is already taken", n)
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

var errNotFound = E("сервер не найден", "server not found")

var errChild = E("это сервер из подписки — менять, выключать и удалять можно только подписку целиком",
	"this server comes from a subscription - edit, disable or delete the subscription instead")

func (s *Store) isChildLocked(id string) bool {
	for _, sv := range s.data.Servers {
		if sv.Sub != nil {
			for _, it := range sv.Sub.Items {
				if it.ID == id {
					return true
				}
			}
		}
	}
	return false
}

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
	if s.isChildLocked(id) {
		return errChild
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
		if sv.Type == "sub" || isSubURL(sv.URI) {
			u, err := validateSubURL(sv.URI)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%q: %v", sv.Name, err))
				continue
			}
			sv.URI, sv.Type, sv.LocalPort, sv.Order = u, "sub", 0, i
			if sv.ID == "" {
				sv.ID = newID()
			}
			sv.Name = s.uniqueName(cleanName(firstNonEmpty(sv.Name, subHost(u))), "")
			if sv.Sub == nil {
				sv.Sub = &SubInfo{}
			}
			var items []Server
			for _, it := range sv.Sub.Items {
				if p, err := ParseURI(it.URI); err == nil {
					it.Type, it.LocalPort, it.Parent = p.Type, 0, ""
					if it.ID == "" {
						it.ID = newID()
					}
					items = append(items, it)
				}
			}
			sv.Sub.Items = nil
			s.data.Servers = append(s.data.Servers, sv)
			last := &s.data.Servers[len(s.data.Servers)-1]
			for _, it := range items {
				it.Name = s.uniqueName(cleanName(firstNonEmpty(it.Name, "server")), "")
				last.Sub.Items = append(last.Sub.Items, it)
			}
			continue
		}
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
		sv := &s.data.Servers[i]
		if sv.Type == "naive" {
			sv.LocalPort = s.freePort(sv.ID)
		}
		if sv.Sub != nil {
			for j := range sv.Sub.Items {
				if sv.Sub.Items[j].Type == "naive" {
					sv.Sub.Items[j].LocalPort = s.freePort(sv.Sub.Items[j].ID)
				}
			}
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
	for _, sv := range s.expandedLocked() {
		if !sv.Enabled || sv.Type == "sub" {
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
	for _, sv := range s.everyLocked() {
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

func (s *Store) portTaken(port int, exceptID string) bool {
	for _, sv := range s.everyLocked() {
		if sv.ID != exceptID && sv.LocalPort == port {
			return true
		}
	}
	return false
}

func (s *Store) freePort(exceptID string) int {
	used := map[int]bool{}
	for _, sv := range s.everyLocked() {
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
