package main

// Вывод консоли в терминал RouterOS: /container shell в WinBox и SSH RouterOS выкидывают
// всё, что не ASCII (кириллица и значки пропадают, ANSI-цвета работают). Поэтому по умолчанию
// консоль печатает ASCII: русский — транслитом, значки — заменами. --utf8 (или LANG=*.UTF-8,
// VPN_UTF8=1) — обычный вывод для нормального терминала.

import (
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// asciiOut — печатать только ASCII (транслит + замены значков).
var asciiOut bool

var translitMap = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "yo", 'ж': "zh", 'з': "z", 'и': "i",
	'й': "y", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t",
	'у': "u", 'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "sch", 'ъ': "", 'ы': "y", 'ь': "",
	'э': "e", 'ю': "yu", 'я': "ya",
	'●': "*", '○': "o", '·': ".", '✕': "x", '✓': "+", '←': "<-", '→': "->", '↓': "v", '↑': "^",
	'…': "...", '—': "-", '–': "-", '«': "\"", '»': "\"", '№': "#", ' ': " ",
	'▁': "_", '▂': ".", '▃': ":", '▄': "-", '▅': "=", '▆': "+", '▇': "*", '█': "#",
}

// translit переводит строку в ASCII. Заглавная кириллица — с заглавной первой буквой (Ж -> Zh).
func translit(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if r < utf8.RuneSelf {
			b.WriteRune(r)
			continue
		}
		if t, ok := translitMap[r]; ok {
			b.WriteString(t)
			continue
		}
		if lr := []rune(strings.ToLower(string(r))); len(lr) == 1 {
			if t, ok := translitMap[lr[0]]; ok && lr[0] != r {
				if t != "" {
					tr := []rune(t)
					b.WriteString(strings.ToUpper(string(tr[0])) + string(tr[1:]))
				}
				continue
			}
		}
		b.WriteByte('?')
	}
	return b.String()
}

// asciiWriter пропускает вывод через translit, не разрывая многобайтовые символы между Write.
type asciiWriter struct {
	w    io.Writer
	tail []byte
}

func (a *asciiWriter) Write(p []byte) (int, error) {
	buf := append(a.tail, p...)
	// хвост — незаконченный UTF-8 символ, допишем со следующим куском
	cut := len(buf)
	for i := len(buf) - 1; i >= 0 && i >= len(buf)-utf8.UTFMax; i-- {
		if utf8.RuneStart(buf[i]) {
			if !utf8.FullRune(buf[i:]) {
				cut = i
			}
			break
		}
	}
	a.tail = append([]byte(nil), buf[cut:]...)
	if _, err := io.WriteString(a.w, translit(string(buf[:cut]))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// setupOutput решает режим вывода и, для ASCII, подменяет os.Stdout/os.Stderr фильтром.
// Возвращает функцию, которая дописывает остаток и восстанавливает потоки.
func setupOutput(forceUTF8, forceASCII bool) func() {
	utf := forceUTF8 || os.Getenv("VPN_UTF8") == "1"
	if !utf && !forceASCII {
		for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
			v := strings.ToLower(os.Getenv(k))
			if strings.Contains(v, "utf-8") || strings.Contains(v, "utf8") {
				utf = true
				break
			}
		}
	}
	asciiOut = !utf || forceASCII
	if !asciiOut {
		return func() {}
	}
	realOut, realErr := os.Stdout, os.Stderr
	stop := []func(){}
	for _, s := range []struct {
		dst **os.File
		to  *os.File
	}{{&os.Stdout, realOut}, {&os.Stderr, realErr}} {
		r, w, err := os.Pipe()
		if err != nil {
			continue
		}
		done := make(chan struct{})
		to := s.to
		go func() {
			aw := &asciiWriter{w: to}
			buf := make([]byte, 4096)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					_, _ = aw.Write(buf[:n])
				}
				if err != nil {
					break
				}
			}
			close(done)
		}()
		*s.dst = w
		dst := s.dst
		stop = append(stop, func() {
			_ = w.Close()
			<-done
			*dst = to
		})
	}
	return func() {
		for _, f := range stop {
			f()
		}
	}
}
