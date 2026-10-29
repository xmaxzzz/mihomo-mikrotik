package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseURI(t *testing.T) {
	cases := []struct {
		in, typ, host, port, name string
		bad                       bool
	}{
		{in: "vless://11111111-2222-3333-4444-555555555555@vpn.example.com:443?security=reality&pbk=abc&sni=x.com&type=tcp#NL%201", typ: "vless", host: "vpn.example.com", port: "443", name: "NL 1"},
		{in: "hy2://pass@1.2.3.4:443,20000-30000/?sni=a.com#HY", typ: "hysteria2", host: "1.2.3.4", port: "443,20000-30000", name: "HY"},
		{in: "hysteria2://pass@[2001:db8::1]:8443#v6", typ: "hysteria2", host: "2001:db8::1", port: "8443", name: "v6"},
		{in: "mierus://u:p@m.example.com?handshake-mode=HANDSHAKE_NO_WAIT&multiplexing=MULTIPLEXING_LOW&port=2012&port=3000-3010&profile=default&protocol=TCP&protocol=UDP#M", typ: "mieru", host: "m.example.com", port: "2012,3000-3010", name: "M"},
		{in: "naive+https://user:pa%40ss@n.example.com:443?insecure-concurrency=2#NV", typ: "naive", host: "n.example.com", port: "443", name: "NV"},
		{in: "naive+quic://user:pass@n.example.com#Q", typ: "naive", host: "n.example.com", name: "Q"},
		{in: "", bad: true},
		{in: "http://x.com", bad: true},
		{in: "vless://host:443", bad: true},
		{in: "mierus://u:p@h?port=1&protocol=TCP&protocol=UDP", bad: true},
		{in: "naive+https://n.example.com", bad: true},
		{in: "vless://id@h:443 #x", bad: true},
	}
	for _, c := range cases {
		p, err := ParseURI(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%q: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if p.Type != c.typ || p.Host != c.host || p.Port != c.port || p.Name != c.name {
			t.Errorf("%q: got %+v", c.in, p)
		}
	}
}

func TestWithNameAndMieruNames(t *testing.T) {
	got := WithName("vless://id@h:443?a=b#old", "Новое имя/1")
	if !strings.HasSuffix(got, "#%D0%9D%D0%BE%D0%B2%D0%BE%D0%B5%20%D0%B8%D0%BC%D1%8F%2F1") {
		t.Fatal(got)
	}
	s := Server{Name: "M", Type: "mieru", URI: "mierus://u:p@h?port=1&port=2-3&protocol=TCP&protocol=UDP"}
	n := ProxyNames(&s)
	if len(n) != 2 || n[0] != "M:1/TCP" || n[1] != "M:2-3/UDP" {
		t.Fatal(n)
	}
}

func TestNaiveArgs(t *testing.T) {
	a, err := NaiveArgs("naive+https://user:pa%40ss@n.example.com:8443?insecure-concurrency=2&padding=true#x", 20800)
	if err != nil {
		t.Fatal(err)
	}
	j := strings.Join(a, " ")
	for _, want := range []string{"--listen=socks://127.0.0.1:20800", "--proxy=https://user:pa%40ss@n.example.com:8443", "--insecure-concurrency=2"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in %q", want, j)
		}
	}
	if strings.Contains(j, "padding") || strings.Contains(j, "#x") {
		t.Errorf("leak: %q", j)
	}
	if r := redactArgs(a); strings.Contains(r, "pa%40ss") {
		t.Errorf("not redacted: %s", r)
	}
}

func TestStoreRender(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir, 20800)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Render(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "servers.txt"))
	if !strings.Contains(string(b), EmptyPlaceholder) || !strings.Contains(string(b), "type: direct") {
		t.Fatalf("empty render: %q", b)
	}
	if _, err := st.Add("hy2://p@a.com:443#A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add("hy2://p@b.com:443#A", ""); err != nil { // дубль имени
		t.Fatal(err)
	}
	nv, err := st.Add("naive+https://u:p@c.com:443#N", "")
	if err != nil {
		t.Fatal(err)
	}
	if nv.LocalPort != 20800 {
		t.Fatalf("port %d", nv.LocalPort)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "servers.txt"))
	dec, err := base64.StdEncoding.DecodeString(string(b))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(dec), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[1], "#A%202") || lines[2] != "socks5://127.0.0.1:20800#N" {
		t.Fatalf("%q", lines)
	}
	st2, err := NewStore(dir, 20800)
	if err != nil || len(st2.List()) != 3 {
		t.Fatalf("reload: %v %d", err, len(st2.List()))
	}
}
