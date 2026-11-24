// rdmcp: servidor MCP para operar máquinas remotas pelo protocolo do RustDesk (stdio ou HTTP) e CLI de teste.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GitDavidJr/rustdesk-mcp/internal/auditoria"
	"github.com/GitDavidJr/rustdesk-mcp/internal/maquinas"
	"github.com/GitDavidJr/rustdesk-mcp/internal/mcpsrv"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/rendezvous"
	"github.com/GitDavidJr/rustdesk-mcp/internal/sessao"
)

var versao = "dev"

func env(k, padrao string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return padrao
}

// padraoDados: /dados no container; na máquina da pessoa, a pasta de configuração dela.
func padraoDados() string {
	if st, err := os.Stat("/dados"); err == nil && st.IsDir() {
		return "/dados"
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "rustdesk-mcp")
	}
	return "dados"
}

func main() {
	log.SetFlags(log.LstdFlags)
	if err := rodar(); err != nil {
		fmt.Fprintln(os.Stderr, "rdmcp:", err)
		os.Exit(1)
	}
}

func rodar() error {
	if len(os.Args) < 2 {
		return errors.New("uso: rdmcp stdio | http | cli <maquina> <executar|tela|raio-x> ...")
	}
	ctx, parar := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer parar()
	dados := env("RDMCP_DADOS", padraoDados())
	if os.Getenv("RDMCP_SERVIDOR") == "" || os.Getenv("RDMCP_CHAVE") == "" {
		return errors.New("RDMCP_SERVIDOR (seu hbbs) e RDMCP_CHAVE (chave pública dele) são obrigatórios")
	}
	regras := os.Getenv("RDMCP_REGRAS_TEXTO")
	if arq := os.Getenv("RDMCP_REGRAS"); arq != "" {
		b, err := os.ReadFile(arq)
		if err != nil {
			return fmt.Errorf("RDMCP_REGRAS: %w", err)
		}
		regras = strings.TrimSpace(regras + "\n" + string(b))
	}
	reg, err := maquinas.Abrir(filepath.Join(dados, "maquinas.json"))
	if err != nil {
		return err
	}
	aud, err := auditoria.Abrir(filepath.Join(dados, "auditoria.jsonl"))
	if err != nil {
		return err
	}
	ocioso, err := time.ParseDuration(env("RDMCP_OCIOSO", "30m"))
	if err != nil {
		return err
	}
	g := &sessao.Gerente{Servidor: rendezvous.Server{IDServer: os.Getenv("RDMCP_SERVIDOR"), Relay: os.Getenv("RDMCP_RELAY"), Key: os.Getenv("RDMCP_CHAVE")}, Cadastro: reg, Ocioso: ocioso, Nome: env("RDMCP_NOME", "Suporte remoto (IA)")}
	defer func() {
		for _, a := range g.Conectadas() {
			a.Fechar()
		}
	}()
	sv := &mcpsrv.Servidor{G: g, Aud: aud, Pasta: filepath.Join(dados, "arquivos"), Versao: versao, Regras: regras}
	switch os.Args[1] {
	case "stdio":
		err := sv.Novo().Run(ctx, &mcp.StdioTransport{})
		if ctx.Err() != nil {
			return nil
		}
		return err
	case "http":
		return servir(ctx, sv)
	case "cli":
		return cli(ctx, g, os.Args[2:])
	}
	return fmt.Errorf("comando %q desconhecido", os.Args[1])
}

func servir(ctx context.Context, sv *mcpsrv.Servidor) error {
	token := os.Getenv("RDMCP_TOKEN")
	if len(token) < 32 {
		return errors.New("RDMCP_TOKEN precisa de pelo menos 32 caracteres: este MCP dá acesso às máquinas remotas")
	}
	srv := sv.Novo()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /saude", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok", versao) })
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if x := r.Header.Get("X-Rdmcp-Token"); x != "" {
			t = x // atrás de um portão que já usa o Authorization
		}
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) != 1 {
			http.Error(w, "não autorizado", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	s := &http.Server{Addr: env("RDMCP_OUVIR", "127.0.0.1:8090"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		fim, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		s.Shutdown(fim)
	}()
	log.Printf("rdmcp %s ouvindo em %s, servidor RustDesk %s", versao, s.Addr, sv.G.Servidor.IDServer)
	if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// cli serve para teste à mão: senha em RDMCP_SENHA.
func cli(ctx context.Context, g *sessao.Gerente, args []string) error {
	fs := flag.NewFlagSet("cli", flag.ContinueOnError)
	cliente := fs.String("cliente", "teste", "")
	aut := fs.String("autorizado", "teste", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	a := fs.Args()
	if len(a) < 2 {
		return errors.New("uso: rdmcp cli <maquina> executar <comando> | tela <arquivo.png>")
	}
	t0 := time.Now()
	m, err := g.Conectar(ctx, sessao.Pedido{Maquina: a[0], Senha: os.Getenv("RDMCP_SENHA"), Cliente: *cliente, Autorizado: *aut})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "conectado em %s: %s %s %s RustDesk %s\n", time.Since(t0).Round(time.Millisecond), m.Info.GetHostname(), m.Info.GetUsername(), m.Info.GetPlatform(), m.Info.GetVersion())
	t0 = time.Now()
	defer func() { fmt.Fprintf(os.Stderr, "levou %s\n", time.Since(t0).Round(time.Millisecond)) }()
	switch a[1] {
	case "executar":
		r, err := m.Executar(ctx, strings.Join(a[2:], " "), "", false, false, 60000, 120*time.Second)
		fmt.Println(r.Saida)
		fmt.Fprintf(os.Stderr, "código %d\n", r.Codigo)
		return err
	case "tela":
		t, err := m.Tela(ctx)
		if err != nil {
			return err
		}
		c, err := t.Capturar(ctx, 0, 0, nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%dx%d %s %d bytes escala %.2f avisos %v\n", c.Largura, c.Altura, c.Mime, len(c.Dados), c.Escala, t.Avisos())
		return os.WriteFile(a[2], c.Dados, 0o600)
	}
	return fmt.Errorf("ação %q desconhecida", a[1])
}
