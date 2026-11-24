package maquinas

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCadastroExigeAutorizacaoEGuardaSoOHash(t *testing.T) {
	arq := filepath.Join(t.TempDir(), "d", "maquinas.json")
	r, err := Abrir(arq)
	if err != nil {
		t.Fatal(err)
	}
	m := Maquina{Apelido: "Loja-Servidor", ID: "123456789", Cliente: "Loja Exemplo", Hash: []byte{1, 2}}
	if r.Salvar(m) == nil {
		t.Fatal("salvou sem quem autorizou")
	}
	m.Autorizado = "dono, 24/11"
	m.Hash = nil
	if r.Salvar(m) == nil {
		t.Fatal("salvou sem credencial")
	}
	m.Hash = []byte{1, 2}
	if err := r.Salvar(m); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(arq)
	if st.Mode().Perm() != 0o600 {
		t.Fatal("cadastro aberto:", st.Mode())
	}
	b, _ := os.ReadFile(arq)
	if strings.Contains(strings.ToLower(string(b)), "senha") {
		t.Fatal("o arquivo não pode ter senha")
	}
	r2, _ := Abrir(arq)
	for _, n := range []string{"loja-servidor", "123 456 789", "123456789"} {
		if x, ok := r2.Achar(n); !ok || x.Cliente != "Loja Exemplo" {
			t.Fatal("não achou por", n)
		}
	}
	if _, ok := r2.Achar("outra"); ok {
		t.Fatal("achou o que não existe")
	}
	if ok, _ := r2.Esquecer("LOJA-SERVIDOR"); !ok {
		t.Fatal("não esqueceu")
	}
}
