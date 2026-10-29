package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// NaiveManager держит по процессу naive на каждый включённый naive-сервер.
type NaiveManager struct {
	bin  string
	uid  int
	dns  string // если задан — адрес сервера резолвится через него (защита от fake-ip петли)
	log  *slog.Logger
	mu   sync.Mutex
	proc map[string]*naiveProc
}

type naiveProc struct {
	key      string
	cancel   context.CancelFunc
	done     chan struct{}
	logs     *ring
	mu       sync.Mutex
	state    string
	restarts int
	started  time.Time
}

type NaiveStatus struct {
	State    string `json:"state"`
	Restarts int    `json:"restarts"`
	Uptime   int64  `json:"uptime_sec"`
}

func NewNaiveManager(bin string, uid int, dns string, l *slog.Logger) *NaiveManager {
	return &NaiveManager{bin: bin, uid: uid, dns: dns, log: l, proc: map[string]*naiveProc{}}
}

func (n *NaiveManager) Available() bool {
	st, err := os.Stat(n.bin)
	return err == nil && st.Mode()&0o111 != 0
}

// Sync приводит запущенные процессы к списку серверов.
func (n *NaiveManager) Sync(servers []Server) {
	want := map[string]Server{}
	for _, s := range servers {
		if s.Type == "naive" && s.Enabled && s.LocalPort > 0 {
			want[s.ID] = s
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for id, p := range n.proc {
		s, ok := want[id]
		if !ok || p.key != naiveKey(s) {
			p.cancel()
			<-p.done
			delete(n.proc, id)
		}
	}
	if !n.Available() {
		if len(want) > 0 {
			n.log.Warn("naive-серверы есть, но бинарь naive отсутствует в образе", "bin", n.bin)
		}
		return
	}
	for id, s := range want {
		if _, ok := n.proc[id]; ok {
			continue
		}
		n.proc[id] = n.start(s)
	}
}

func naiveKey(s Server) string { return fmt.Sprintf("%d|%s", s.LocalPort, s.URI) }

func (n *NaiveManager) start(s Server) *naiveProc {
	ctx, cancel := context.WithCancel(context.Background())
	p := &naiveProc{key: naiveKey(s), cancel: cancel, done: make(chan struct{}), logs: newRing(300), state: "starting"}
	go func() {
		defer close(p.done)
		backoff := time.Second
		for {
			args, err := NaiveArgs(s.URI, s.LocalPort)
			if err != nil {
				p.setState("error: " + err.Error())
				p.logs.Add("config error: " + err.Error())
				<-ctx.Done()
				return
			}
			args = append(args, n.resolverArgs(ctx, s.URI, p.logs)...)
			cmd := exec.CommandContext(ctx, n.bin, args...)
			cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
			cmd.WaitDelay = 5 * time.Second
			if n.uid > 0 {
				// отдельный uid: в TUN-режиме mihomo исключает его из auto-route (exclude-uid),
				// иначе исходящий трафик naive зациклится обратно в mihomo
				cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(n.uid), Gid: uint32(n.uid)}}
			}
			pr, pw := io.Pipe()
			cmd.Stdout, cmd.Stderr = pw, pw
			go p.logs.Consume(pr)
			p.logs.Add(fmt.Sprintf("start: naive %s", redactArgs(args)))
			start := time.Now()
			if err := cmd.Start(); err != nil {
				pw.Close()
				p.setState("error: " + err.Error())
				p.logs.Add("start failed: " + err.Error())
			} else {
				p.mu.Lock()
				p.state, p.started = "running", start
				p.mu.Unlock()
				n.log.Info("naive started", "server", s.Name, "port", s.LocalPort, "pid", cmd.Process.Pid)
				err := cmd.Wait()
				pw.Close()
				if ctx.Err() != nil {
					n.log.Info("naive stopped", "server", s.Name)
					return
				}
				p.logs.Add(fmt.Sprintf("exited: %v", err))
				n.log.Warn("naive exited, restarting", "server", s.Name, "err", err)
				p.setState("restarting")
			}
			if time.Since(start) > time.Minute {
				backoff = time.Second
			}
			p.mu.Lock()
			p.restarts++
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()
	return p
}

// resolverArgs: при заданном NAIVE_DNS резолвим хост сервера мимо системного DNS
// и передаём в naive через --host-resolver-rules (SNI/Host остаются доменом).
func (n *NaiveManager) resolverArgs(ctx context.Context, uri string, logs *ring) []string {
	host := NaiveHost(uri)
	if n.dns == "" || host == "" || isIP(host) {
		return nil
	}
	dnsAddr := n.dns
	if _, _, err := net.SplitHostPort(dnsAddr); err != nil {
		dnsAddr = net.JoinHostPort(dnsAddr, "53")
	}
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 3 * time.Second}
		return d.DialContext(ctx, network, dnsAddr)
	}}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := r.LookupIPAddr(cctx, host)
	if err != nil || len(ips) == 0 {
		logs.Add(fmt.Sprintf("resolve %s via %s failed: %v (используется системный DNS)", host, dnsAddr, err))
		return nil
	}
	ip := ips[0].IP
	for _, a := range ips {
		if a.IP.To4() != nil {
			ip = a.IP
			break
		}
	}
	logs.Add(fmt.Sprintf("resolve %s -> %s via %s", host, ip, dnsAddr))
	return []string{fmt.Sprintf("--host-resolver-rules=MAP %s %s", host, ip)}
}

func (n *NaiveManager) Status(id string) (NaiveStatus, bool) {
	n.mu.Lock()
	p, ok := n.proc[id]
	n.mu.Unlock()
	if !ok {
		return NaiveStatus{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := NaiveStatus{State: p.state, Restarts: p.restarts}
	if p.state == "running" {
		st.Uptime = int64(time.Since(p.started).Seconds())
	}
	return st, true
}

func (n *NaiveManager) Logs(id string) []string {
	n.mu.Lock()
	p, ok := n.proc[id]
	n.mu.Unlock()
	if !ok {
		return nil
	}
	return p.logs.Lines()
}

func (n *NaiveManager) StopAll() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for id, p := range n.proc {
		p.cancel()
		<-p.done
		delete(n.proc, id)
	}
}

func (p *naiveProc) setState(s string) {
	p.mu.Lock()
	p.state = s
	p.mu.Unlock()
}

func redactArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "--proxy=") {
			if at := strings.LastIndexByte(a, '@'); at > 0 {
				if sc := strings.Index(a, "://"); sc > 0 && sc < at {
					a = a[:sc+3] + "***" + a[at:]
				}
			}
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

// ring — кольцевой буфер строк лога.
type ring struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newRing(max int) *ring { return &ring{max: max} }

func (r *ring) Add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, time.Now().Format("15:04:05 ")+s)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

func (r *ring) Consume(rd io.Reader) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		r.Add(sc.Text())
	}
}

func (r *ring) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}
