// Package maquinas é o cadastro das máquinas de cliente com acesso fixo autorizado.
package maquinas

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Maquina cadastrada. A senha não é guardada: fica o hash salgado do RustDesk, que só entra nela e
// deixa de valer quando a senha muda.
type Maquina struct {
	Apelido    string    `json:"apelido"`
	ID         string    `json:"id"`
	Cliente    string    `json:"cliente"`
	Autorizado string    `json:"autorizado_por"`
	Em         time.Time `json:"em"`
	Obs        string    `json:"obs,omitempty"`
	Hash       []byte    `json:"hash"`
}

// Registro em arquivo (0600).
type Registro struct {
	mu      sync.Mutex
	caminho string
	lista   map[string]Maquina
}

// Abrir carrega o cadastro; arquivo ausente é cadastro vazio.
func Abrir(caminho string) (*Registro, error) {
	r := &Registro{caminho: caminho, lista: map[string]Maquina{}}
	b, err := os.ReadFile(caminho)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &r.lista); err != nil {
		return nil, fmt.Errorf("cadastro ilegível em %s: %w", caminho, err)
	}
	return r, nil
}

func chave(apelido string) string { return strings.ToLower(strings.TrimSpace(apelido)) }

// Achar por apelido ou pelo ID.
func (r *Registro) Achar(nome string) (Maquina, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.lista[chave(nome)]; ok {
		return m, true
	}
	id := SoNumeros(nome)
	for _, m := range r.lista {
		if id != "" && m.ID == id {
			return m, true
		}
	}
	return Maquina{}, false
}

// Listar em ordem de apelido.
func (r *Registro) Listar() []Maquina {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Maquina, 0, len(r.lista))
	for _, m := range r.lista {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Apelido < out[j].Apelido })
	return out
}

// Salvar cadastra ou atualiza.
func (r *Registro) Salvar(m Maquina) error {
	if chave(m.Apelido) == "" || SoNumeros(m.ID) != m.ID || m.ID == "" {
		return errors.New("apelido e ID são obrigatórios")
	}
	if strings.TrimSpace(m.Cliente) == "" || strings.TrimSpace(m.Autorizado) == "" {
		return errors.New("cliente e quem autorizou são obrigatórios: acesso fixo só com o ok do cliente")
	}
	if len(m.Hash) == 0 {
		return errors.New("sem hash da senha: conecte com a senha fixa antes de salvar")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lista[chave(m.Apelido)] = m
	return r.gravar()
}

// Esquecer remove o cadastro.
func (r *Registro) Esquecer(apelido string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.lista[chave(apelido)]; !ok {
		return false, nil
	}
	delete(r.lista, chave(apelido))
	return true, r.gravar()
}

func (r *Registro) gravar() error {
	if err := os.MkdirAll(filepath.Dir(r.caminho), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r.lista, "", " ")
	if err != nil {
		return err
	}
	tmp := r.caminho + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.caminho)
}

// SoNumeros tira espaços do ID como o RustDesk mostra ("123 456 789").
func SoNumeros(id string) string {
	return strings.Map(func(c rune) rune {
		if c >= '0' && c <= '9' {
			return c
		}
		return -1
	}, id)
}
