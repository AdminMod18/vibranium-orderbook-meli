package domain

import (
	"strings"
	"testing"
)

func TestValidID(t *testing.T) {
	valid := []string{"a", "alice", "user@example.com", "o-1.2:3_x+y", "A1", strings.Repeat("x", MaxIDLen)}
	invalid := map[string]string{
		"vacío":               "",
		"65 caracteres":       strings.Repeat("x", MaxIDLen+1),
		"con espacio":         "a b",
		"con barra":           "a/b",
		"con porcentaje":      "a%20b",
		"unicode":             "ñandú",
		"carácter de control": "a\x00b",
		"salto de línea":      "a\nb",
		"comillas":            `a"b`,
		"400 KB":              strings.Repeat("X", 400_000),
	}
	for _, s := range valid {
		if !ValidID(s) {
			t.Errorf("%q debería ser válido", s)
		}
	}
	for name, s := range invalid {
		if ValidID(s) {
			t.Errorf("%s debería ser inválido", name)
		}
	}
}
