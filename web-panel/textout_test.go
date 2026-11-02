package main

import (
	"bytes"
	"testing"
)

func TestTranslit(t *testing.T) {
	cases := map[string]string{
		"Трафик через: DE":          "Trafik cherez: DE",
		"Жёлтый ЩИТ, Юг":            "Zhyoltyy SchIT, Yug",
		"● ok ✕ → ↓ 1,2 Мбит/с":     "* ok x -> v 1,2 Mbit/s",
		"«Нет серверов (напрямую)»": "\"Net serverov (napryamuyu)\"",
		"plain ascii": "plain ascii",
		"▁▄█":         "_-#",
		"日":           "?",
	}
	for in, want := range cases {
		if got := translit(in); got != want {
			t.Errorf("translit(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestASCIIWriterSplitRune(t *testing.T) {
	var out bytes.Buffer
	w := &asciiWriter{w: &out}
	src := []byte("Да → ок\n")
	// режем посреди многобайтовых символов
	for i := 0; i < len(src); i += 3 {
		end := i + 3
		if end > len(src) {
			end = len(src)
		}
		if _, err := w.Write(src[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	if got := out.String(); got != "Da -> ok\n" {
		t.Fatalf("got %q", got)
	}
}

func TestPadASCIIWidth(t *testing.T) {
	old := asciiOut
	asciiOut = true
	defer func() { asciiOut = old }()
	if got := pad("Жуковка", 10); got != "Zhukovka  " {
		t.Fatalf("pad: %q", got)
	}
	if got := pad("Щёлково-Север", 8); got != "Schyolk~" {
		t.Fatalf("cut: %q", got)
	}
	if got := padL("39 мс", 7); got != "  39 ms" {
		t.Fatalf("padL: %q", got)
	}
	// английский интерфейс в ASCII-режиме
	if delayText(39) != "39 ms" || delayText(0) != "no link" || modeName("DIRECT") != "no VPN" {
		t.Fatalf("en: %q %q %q", delayText(39), delayText(0), modeName("DIRECT"))
	}
	if got := eventText(Event{Msg: "нет связи с DE", MsgEN: "DE is DOWN"}); got != "DE is DOWN" {
		t.Fatalf("event en: %q", got)
	}
	asciiOut = false
	if delayText(39) != "39 мс" || eventText(Event{Msg: "нет связи с DE", MsgEN: "DE is DOWN"}) != "нет связи с DE" {
		t.Fatal("ru")
	}
}
