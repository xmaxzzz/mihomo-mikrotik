package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// как отдаёт панель RIXXX (generic): base64 списка naive+https / mierus / hysteria2 / tt
const flowerLike = "naive+https://max:p%40ss@flower.example:443#Naive\n" +
	"mierus://max:pw@flower.example?handshake-mode=HANDSHAKE_NO_WAIT&mtu=1400&multiplexing=MULTIPLEXING_LOW&port=2012-2022&profile=default&protocol=TCP#Mieru\n" +
	"mierus://max:pw@flower.example?port=32012&protocol=TCP&profile=default#Mieru 32012\n" +
	"hysteria2://max:pw@flower.example:443?sni=flower.example#HY2\n" +
	"tt://max:pw@flower.example:9443#TT\n"

func TestParseSubBody(t *testing.T) {
	res, err := parseSubBody([]byte(base64.StdEncoding.EncodeToString([]byte(flowerLike))))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 4 || res.Skipped["tt"] != 1 {
		t.Fatalf("items %d skipped %v", len(res.Items), res.Skipped)
	}
	if res.Items[0].Type != "naive" || res.Items[0].Name != "Naive" || res.Items[3].Type != "hysteria2" {
		t.Fatalf("items: %+v", res.Items)
	}
	// plain text тоже
	if res, err := parseSubBody([]byte(flowerLike)); err != nil || len(res.Items) != 4 {
		t.Fatalf("plain: %v %v", res, err)
	}
	// JSON и пусто — понятные ошибки
	if _, err := parseSubBody([]byte(`{"outbounds":[]}`)); err == nil || !strings.Contains(errText(err, true), "JSON") {
		t.Fatalf("json: %v", err)
	}
	if _, err := parseSubBody([]byte(base64.StdEncoding.EncodeToString([]byte("tt://a@b:1\n")))); err == nil ||
		!strings.Contains(errText(err, true), "tt://") {
		t.Fatalf("only tt: %v", err)
	}
}

func TestFetchSubHeaders(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		w.Header().Set("Subscription-Userinfo", "upload=0; download=1048576; total=0; expire=1800000000")
		w.Header().Set("Profile-Update-Interval", "24")
		_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString([]byte(flowerLike))))
	}))
	defer srv.Close()
	res, err := FetchSub(context.Background(), srv.URL+"/sub/token", "", "mihomo-web/test")
	if err != nil {
		t.Fatal(err)
	}
	if ua != "mihomo-web/test" || res.IntervalH != 24 || res.Download != 1048576 || res.Expire != 1800000000 || len(res.Items) != 4 {
		t.Fatalf("ua %q res %+v", ua, res)
	}
	// ошибка сети не светит токен подписки
	_, err = FetchSub(context.Background(), "http://127.0.0.1:1/sub/SECRET", "", "x")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("leak: %v", err)
	}
}

func TestStoreSubLifecycle(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir, 20800)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := st.Add("hy2://p@a.example:443#Solo", "")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := st.Add("https://sub.example/sub/abc", "")
	if err != nil || sub.Type != "sub" || sub.Name != "sub.example" {
		t.Fatalf("add sub: %+v %v", sub, err)
	}
	if _, err := st.Add("https://sub.example/sub/abc", ""); err == nil {
		t.Fatal("duplicate subscription accepted")
	}
	res, _ := parseSubBody([]byte(flowerLike))
	changed, err := st.SetSubResult(sub.ID, res, nil)
	if err != nil || !changed {
		t.Fatalf("set: %v %v", changed, err)
	}
	exp := st.Expanded()
	if len(exp) != 6 || exp[1].Type != "sub" || exp[2].Parent != sub.ID || exp[2].Type != "naive" || exp[2].LocalPort == 0 {
		t.Fatalf("expanded: %+v", exp)
	}
	naive := exp[2]
	if naive.LocalPort == 20800 && plain.LocalPort == 20800 {
		t.Fatal("port clash")
	}

	// повторное обновление тем же набором: ничего не меняется, порт и ID naive те же
	changed, _ = st.SetSubResult(sub.ID, res, nil)
	if changed {
		t.Fatal("same content reported as changed")
	}
	if got, _ := st.Get(naive.ID); got.LocalPort != naive.LocalPort || got.Parent != sub.ID {
		t.Fatalf("naive kept: %+v vs %+v", got, naive)
	}

	// ошибка обновления: старые серверы остаются, ошибка записана на двух языках
	if _, err := st.SetSubResult(sub.ID, nil, E("недоступна", "unreachable")); err != nil {
		t.Fatal(err)
	}
	s2, _ := st.Get(sub.ID)
	if s2.Sub.Error != "недоступна" || s2.Sub.ErrorEN != "unreachable" || len(s2.Sub.Items) != 4 {
		t.Fatalf("after error: %+v", s2.Sub)
	}

	// серверы подписки нельзя править/удалять по отдельности
	if _, err := st.Update(naive.ID, Patch{Name: ptr("x")}); !errors.Is(err, errChild) {
		t.Fatalf("update child: %v", err)
	}
	if err := st.Delete(naive.ID); !errors.Is(err, errChild) {
		t.Fatalf("delete child: %v", err)
	}

	// рендер для mihomo: naive как локальный socks5, mieru/hy2 как есть, tt нет
	b, _ := os.ReadFile(filepath.Join(dir, "servers.txt"))
	dec, _ := base64.StdEncoding.DecodeString(string(b))
	txt := string(dec)
	if !strings.Contains(txt, "socks5://127.0.0.1:") || !strings.Contains(txt, "mierus://") || strings.Contains(txt, "tt://") ||
		!strings.Contains(txt, "#Solo") || strings.Contains(txt, "https://sub.example") {
		t.Fatalf("render:\n%s", txt)
	}

	// выключенная подписка выключает свои серверы
	off := false
	if _, err := st.Update(sub.ID, Patch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if n := len(st.Active()); n != 1 {
		t.Fatalf("active with sub off: %d", n)
	}
	// удаление подписки убирает и её серверы
	if err := st.Delete(sub.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(st.Expanded()); n != 1 {
		t.Fatalf("after delete: %d", n)
	}
}

func TestSubDue(t *testing.T) {
	now := time.Now()
	sv := Server{Type: "sub", Enabled: true}
	if !subDue(sv, now, 12*time.Hour, 30*time.Minute) {
		t.Fatal("never fetched must be due")
	}
	sv.Sub = &SubInfo{CheckedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour), IntervalH: 24}
	if subDue(sv, now, 12*time.Hour, 30*time.Minute) {
		t.Fatal("fresh sub is due")
	}
	sv.Sub.Error = "x"
	if !subDue(sv, now, 12*time.Hour, 30*time.Minute) {
		t.Fatal("failed sub must retry after 30 min")
	}
	sv.Sub.CheckedAt = now.Add(-time.Minute)
	if subDue(sv, now, 12*time.Hour, 30*time.Minute) {
		t.Fatal("failed sub retried too early")
	}
}

func ptr[T any](v T) *T { return &v }
