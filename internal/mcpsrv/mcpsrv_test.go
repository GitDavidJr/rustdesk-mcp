package mcpsrv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GitDavidJr/rustdesk-mcp/internal/auditoria"
	"github.com/GitDavidJr/rustdesk-mcp/internal/maquinas"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/rendezvous"
	"github.com/GitDavidJr/rustdesk-mcp/internal/sessao"
)

func abrir(t *testing.T) (*mcp.ClientSession, string) {
	t.Helper()
	d := t.TempDir()
	reg, _ := maquinas.Abrir(filepath.Join(d, "m.json"))
	aud, err := auditoria.Abrir(filepath.Join(d, "a.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sv := &Servidor{G: &sessao.Gerente{Cadastro: reg, Servidor: rendezvous.Server{IDServer: "127.0.0.1:1", Key: "x"}}, Aud: aud, Pasta: d, Versao: "t"}
	c, s := mcp.NewInMemoryTransports()
	if _, err := sv.Novo().Connect(context.Background(), s, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t"}, nil).Connect(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	return cs, filepath.Join(d, "a.jsonl")
}

func TestFerramentasERegras(t *testing.T) {
	cs, _ := abrir(t)
	if !strings.Contains(cs.InitializeResult().Instructions, "Só entre com o ok do dono da máquina") {
		t.Fatal("as regras têm de ir na instrução do servidor")
	}
	l, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	nomes := map[string]*mcp.Tool{}
	for _, f := range l.Tools {
		nomes[f.Name] = f
	}
	for _, n := range []string{"maquinas", "conectar", "desconectar", "maquina_salvar", "maquina_esquecer", "raio_x", "executar", "terminal_escrever", "terminal_ler", "tela", "clicar", "arrastar", "rolar", "digitar", "teclas", "arquivos", "arquivo_baixar", "arquivo_enviar", "http"} {
		if nomes[n] == nil {
			t.Fatal("falta", n)
		}
	}
	if nomes["executar"].Annotations.ReadOnlyHint || !nomes["raio_x"].Annotations.ReadOnlyHint {
		t.Fatal("anotação de leitura trocada")
	}
}

func TestSenhaNuncaVaiParaAAuditoria(t *testing.T) {
	cs, aud := abrir(t)
	ctx := context.Background()
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "conectar", Arguments: map[string]any{"maquina": "123456789", "senha": "SEGREDO1", "cliente": "Loja", "autorizado_por": "dono", "usuario_windows": "adm", "senha_windows": "SEGREDO2"}})
	if err != nil || !r.IsError {
		t.Fatal("devia falhar sem servidor", err)
	}
	cs.CallTool(ctx, &mcp.CallToolParams{Name: "digitar", Arguments: map[string]any{"maquina": "123456789", "texto": "SEGREDO3", "sensivel": true}})
	cs.CallTool(ctx, &mcp.CallToolParams{Name: "executar", Arguments: map[string]any{"maquina": "123456789", "comando": "Get-Date"}})
	b, _ := os.ReadFile(aud)
	for _, s := range []string{"SEGREDO1", "SEGREDO2", "SEGREDO3"} {
		if strings.Contains(string(b), s) {
			t.Fatal("vazou", s)
		}
	}
	if n := strings.Count(string(b), "\n"); n != 3 || !strings.Contains(string(b), "Get-Date") {
		t.Fatal("auditoria incompleta:", string(b))
	}
}

func TestConectarExigeClienteEAutorizacao(t *testing.T) {
	cs, _ := abrir(t)
	for _, a := range []map[string]any{{"maquina": "123456789"}, {"maquina": "123456789", "senha": "x"}, {"maquina": "123456789", "senha": "x", "cliente": "Loja"}, {"maquina": "apelido-inexistente"}} {
		r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "conectar", Arguments: a})
		if err != nil || !r.IsError {
			t.Fatal("aceitou", a)
		}
	}
}

func TestDecodificaArquivoAntigo(t *testing.T) {
	for _, b := range [][]byte{[]byte("ação"), {'a', 0xe7, 0xe3, 'o'}, {0xff, 0xfe, 'a', 0, 0xe7, 0, 0xe3, 0, 'o', 0}} {
		if s := decodifica(b); s != "ação" {
			t.Fatalf("%q de %v", s, b)
		}
	}
}
