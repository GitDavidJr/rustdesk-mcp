// Package auditoria registra tudo o que foi feito na máquina do cliente: quando, em qual e o quê.
package auditoria

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Log acrescenta uma linha JSON por ação. Nunca recebe senha nem texto marcado como sensível.
type Log struct {
	mu sync.Mutex
	f  *os.File
}

// Abrir cria ou continua o arquivo (só o dono lê).
func Abrir(caminho string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(caminho), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(caminho, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{f: f}, nil
}

// Registro de uma ação.
type Registro struct {
	Em      time.Time      `json:"em"`
	Maquina string         `json:"maquina"`
	Cliente string         `json:"cliente,omitempty"`
	Acao    string         `json:"acao"`
	Dados   map[string]any `json:"dados,omitempty"`
	Erro    string         `json:"erro,omitempty"`
	MS      int64          `json:"ms,omitempty"`
}

// Anotar grava o registro.
func (l *Log) Anotar(r Registro) {
	if l == nil {
		return
	}
	if r.Em.IsZero() {
		r.Em = time.Now()
	}
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.f.Write(append(b, '\n'))
}
