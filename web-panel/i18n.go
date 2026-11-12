package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// uErr — ошибка для пользователя на двух языках. Error() — русский (веб, журнал);
// консоль в терминале RouterOS шлёт Accept-Language: en и получает английский.
type uErr struct {
	ru, en string
}

func (e *uErr) Error() string { return e.ru }

// E — двуязычная ошибка.
func E(ru, en string) error { return &uErr{ru: ru, en: en} }

// Ef — двуязычная ошибка с форматированием (одинаковые аргументы для обоих шаблонов).
func Ef(ru, en string, a ...any) error {
	return &uErr{ru: fmt.Sprintf(ru, a...), en: fmt.Sprintf(en, a...)}
}

// errText — текст ошибки на нужном языке; у обычных ошибок — как есть.
func errText(err error, en bool) string {
	if err == nil {
		return ""
	}
	var u *uErr
	if en && errors.As(err, &u) {
		return u.en
	}
	return err.Error()
}

// wantEN — клиент просит английский (консоль в ASCII-режиме).
func wantEN(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Accept-Language"))), "en")
}

// tr — строка ответа на языке клиента.
func tr(r *http.Request, ru, en string) string {
	if wantEN(r) {
		return en
	}
	return ru
}
