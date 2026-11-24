package sessao

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GitDavidJr/rustdesk-mcp/internal/maquinas"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/files"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/forward"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/pb"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/peer"
	"github.com/GitDavidJr/rustdesk-mcp/internal/rd/rendezvous"
)

// Gerente guarda as máquinas conectadas. Uma por ID, compartilhada por quem usa o MCP.
type Gerente struct {
	Servidor rendezvous.Server
	Cadastro *maquinas.Registro
	Ocioso   time.Duration
	// Nome que aparece para a pessoa na janela de conexão do RustDesk.
	Nome string

	mu     sync.Mutex
	ativas map[string]*Ativa
}

// Pedido de conexão.
type Pedido struct {
	Maquina                     string // apelido cadastrado ou ID
	Senha                       string // vazia: usa o acesso fixo cadastrado
	Cliente, Autorizado         string
	UsuarioWindows, SenhaWindow string // administrador do Windows: libera o terminal de administrador
}

// Ativa é uma máquina conectada; terminal, tela e arquivos abrem quando usados.
type Ativa struct {
	ID, Apelido, Cliente, Autorizado string
	g                                *Gerente
	senha                            string
	hash                             []byte
	usuario, senhaWin                string
	sessao                           uint64

	cred sync.Mutex // senha, hash e Info

	mu        sync.Mutex
	term      *Terminal
	admin     *Terminal
	conversa  *Terminal
	tela      *Tela
	arq       *files.Client
	tuneis    map[string]*forward.Mapping
	Info      *pb.PeerInfo
	ultimoUso time.Time
}

type semPergunta struct{}

func (semPergunta) Password(errada bool) (string, error) {
	if errada {
		return "", errors.New("senha recusada pela máquina (a de uso único muda depois de cada entrada)")
	}
	return "", errors.New("a máquina pede senha e não há acesso fixo cadastrado")
}
func (semPergunta) TwoFactor(bool) (string, error) {
	return "", errors.New("a máquina pede código de duas etapas")
}
func (semPergunta) Status(string) {}

// Conectar valida a senha abrindo o terminal (ou a tela, se o terminal não estiver disponível).
func (g *Gerente) Conectar(ctx context.Context, p Pedido) (*Ativa, error) {
	m, cadastrada := g.Cadastro.Achar(p.Maquina)
	id := m.ID
	if !cadastrada {
		id = maquinas.SoNumeros(p.Maquina)
		if id == "" {
			return nil, fmt.Errorf("máquina %q não cadastrada e não é um ID", p.Maquina)
		}
		if p.Senha == "" {
			return nil, errors.New("máquina sem acesso fixo: informe a senha que o cliente mandou")
		}
		if strings.TrimSpace(p.Cliente) == "" || strings.TrimSpace(p.Autorizado) == "" {
			return nil, errors.New("informe o cliente e quem autorizou o acesso")
		}
	}
	a := &Ativa{ID: id, Apelido: m.Apelido, Cliente: m.Cliente, Autorizado: m.Autorizado, g: g, senha: p.Senha, usuario: p.UsuarioWindows, senhaWin: p.SenhaWindow, sessao: peer.NovaSessao(), tuneis: map[string]*forward.Mapping{}, ultimoUso: time.Now()}
	if p.Senha == "" {
		a.hash = m.Hash
	}
	if p.Cliente != "" {
		a.Cliente = p.Cliente
	}
	if p.Autorizado != "" {
		a.Autorizado = p.Autorizado
	}
	if velha := g.pegar(id); velha != nil {
		velha.Fechar()
	}
	_, errT := a.Terminal(ctx)
	if errT != nil {
		if pe := (*peer.Error)(nil); errors.As(errT, &pe) && (pe.Code == peer.CodeWrongPassword || pe.Code == peer.CodeCancelled) {
			return nil, errT
		}
		if _, errTela := a.Tela(ctx); errTela != nil {
			return nil, fmt.Errorf("terminal: %v; tela: %v", errT, errTela)
		}
	}
	g.mu.Lock()
	if g.ativas == nil {
		g.ativas = map[string]*Ativa{}
	}
	g.ativas[id] = a
	g.mu.Unlock()
	go a.vigiar()
	return a, nil
}

func (g *Gerente) pegar(id string) *Ativa { g.mu.Lock(); defer g.mu.Unlock(); return g.ativas[id] }

// Pegar a máquina conectada, por apelido ou ID.
func (g *Gerente) Pegar(nome string) (*Ativa, error) {
	id := maquinas.SoNumeros(nome)
	if m, ok := g.Cadastro.Achar(nome); ok {
		id = m.ID
	}
	a := g.pegar(id)
	if a == nil {
		return nil, fmt.Errorf("%s não está conectada: use conectar", nome)
	}
	a.mu.Lock()
	a.ultimoUso = time.Now()
	a.mu.Unlock()
	return a, nil
}

// Conectadas agora.
func (g *Gerente) Conectadas() []*Ativa {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*Ativa, 0, len(g.ativas))
	for _, a := range g.ativas {
		out = append(out, a)
	}
	return out
}

func (a *Ativa) vigiar() {
	for {
		time.Sleep(time.Minute)
		if a.g.pegar(a.ID) != a {
			return
		}
		a.mu.Lock()
		parado := time.Since(a.ultimoUso)
		a.mu.Unlock()
		if a.g.Ocioso > 0 && parado > a.g.Ocioso {
			a.Fechar()
			return
		}
	}
}

func (a *Ativa) conectar(ctx context.Context, tipo pb.ConnType, login func(*pb.LoginRequest), codigo func(string) peer.Code) (*peer.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	a.cred.Lock()
	senha, hash := a.senha, a.hash
	a.cred.Unlock()
	s, err := peer.Connect(ctx, peer.Options{
		Server: a.g.Servidor, PeerID: a.ID, ConnType: tipo, Login: login, Password: senha, RememberedHash: hash,
		Prompter: semPergunta{}, MyID: "rustdesk-mcp", MyName: a.g.Nome, MyPlatform: "Linux", Version: "1.5.3",
		SessionID: a.sessao, LoginErrorCode: codigo,
	})
	if err != nil {
		return nil, err
	}
	a.cred.Lock()
	defer a.cred.Unlock()
	a.Info = s.PeerInfo
	if len(s.Hash) > 0 {
		a.hash, a.senha = s.Hash, "" // daqui em diante basta o hash; a senha sai da memória
	}
	return s, nil
}

func codigoTerminal(e string) peer.Code {
	if l := strings.ToLower(e); strings.Contains(l, "no permission") || strings.Contains(l, "not allowed") {
		return peer.CodePermissionDenied
	}
	return peer.CodeLogin
}

// Terminal do usuário logado, reaberto se caiu.
func (a *Ativa) Terminal(ctx context.Context) (*Terminal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.term != nil && a.term.Vivo() {
		return a.term, nil
	}
	s, err := a.conectar(ctx, pb.ConnType_TERMINAL, LoginTerminal("", ""), codigoTerminal)
	if err != nil {
		return nil, err
	}
	a.term, err = AbrirTerminal(ctx, s)
	return a.term, err
}

// Conversa é o terminal interativo, separado do que roda comandos: um sqlcmd aberto não atrapalha o executar.
func (a *Ativa) Conversa(ctx context.Context) (*Terminal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conversa != nil && a.conversa.Vivo() {
		return a.conversa, nil
	}
	s, err := a.conectar(ctx, pb.ConnType_TERMINAL, LoginTerminal("", ""), codigoTerminal)
	if err != nil {
		return nil, err
	}
	a.conversa, err = AbrirTerminal(ctx, s)
	return a.conversa, err
}

// TerminalAdmin roda como SYSTEM; exige usuário e senha de um administrador do Windows informados no conectar.
func (a *Ativa) TerminalAdmin(ctx context.Context) (*Terminal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.admin != nil && a.admin.Vivo() {
		return a.admin, nil
	}
	if a.usuario == "" {
		return nil, errors.New("terminal de administrador precisa do usuário e senha de um administrador do Windows no conectar; sem isso, abra o PowerShell como administrador pela tela e clique Sim no aviso")
	}
	s, err := a.conectar(ctx, pb.ConnType_TERMINAL, LoginTerminal(a.usuario, a.senhaWin), codigoTerminal)
	if err != nil {
		return nil, err
	}
	a.admin, err = AbrirTerminal(ctx, s)
	return a.admin, err
}

// Tela da máquina, reaberta se caiu.
func (a *Ativa) Tela(ctx context.Context) (*Tela, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tela != nil && a.tela.Vivo() {
		return a.tela, nil
	}
	s, err := a.conectar(ctx, pb.ConnType_DEFAULT_CONN, LoginTela, nil)
	if err != nil {
		return nil, err
	}
	a.tela = AbrirTela(s)
	return a.tela, nil
}

// Arquivos: sessão de transferência.
func (a *Ativa) Arquivos(ctx context.Context) (*files.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.arq != nil {
		if a.arq.Probe(ctx, 4*time.Second) == nil {
			return a.arq, nil
		}
		a.arq.Close()
		a.arq = nil
	}
	s, err := a.conectar(ctx, pb.ConnType_FILE_TRANSFER, files.LoginFields("", true), files.LoginErrorCode)
	if err != nil {
		return nil, err
	}
	a.arq = files.New(s, nil)
	return a.arq, nil
}

// Enviar grava conteúdo num arquivo da máquina.
func (a *Ativa) Enviar(ctx context.Context, conteudo []byte, destino string) error {
	c, err := a.Arquivos(ctx)
	if err != nil {
		return err
	}
	d, err := os.MkdirTemp("", "rdmcp")
	if err != nil {
		return err
	}
	defer os.RemoveAll(d)
	nome := c.Base(destino)
	local := filepath.Join(d, filepath.Base(nome))
	if err := os.WriteFile(local, conteudo, 0o600); err != nil {
		return err
	}
	return c.Upload(ctx, local, destino, files.TransferOptions{})
}

// Baixar traz um arquivo da máquina para o disco local.
func (a *Ativa) Baixar(ctx context.Context, origem, local string) error {
	c, err := a.Arquivos(ctx)
	if err != nil {
		return err
	}
	return c.Download(ctx, origem, local, files.TransferOptions{})
}

// Executar roda o comando no terminal do usuário (ou de administrador). Comando longo vai como arquivo temporário.
func (a *Ativa) Executar(ctx context.Context, comando, pasta string, admin bool, maxSaida int, prazo time.Duration) (Resultado, error) {
	abrir := a.Terminal
	if admin {
		abrir = a.TerminalAdmin
	}
	t, err := abrir(ctx)
	if err != nil {
		return Resultado{}, err
	}
	r, err := t.Executar(ctx, comando, pasta, maxSaida, prazo)
	if !errors.Is(err, ErrComandoLongo) {
		return r, err
	}
	tmp, err := t.Executar(ctx, se(t.windows, "[IO.Path]::GetTempPath()", "echo ${TMPDIR:-/tmp}/"), "", 1000, 30*time.Second)
	if err != nil {
		return Resultado{}, err
	}
	id := marca()
	destino := strings.TrimSpace(tmp.Saida) + "rdmcp-" + id + se(t.windows, ".ps1", ".sh")
	corpo := []byte(se(t.windows, string(rune(0xFEFF)), "") + comando)
	if err := a.Enviar(ctx, corpo, destino); err != nil {
		return Resultado{}, fmt.Errorf("comando longo: não consegui enviar o arquivo: %w", err)
	}
	q := "'" + strings.ReplaceAll(destino, "'", "''") + "'"
	if t.windows {
		return t.Executar(ctx, "try { & "+q+" } finally { Remove-Item -LiteralPath "+q+" -Force -ErrorAction SilentlyContinue }", pasta, maxSaida, prazo)
	}
	return t.Executar(ctx, "sh '"+destino+"'; c=$?; rm -f '"+destino+"'; (exit $c)", pasta, maxSaida, prazo)
}

func se(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

// Discar abre uma conexão TCP a partir da máquina do cliente (a rede dele), pelo túnel do RustDesk.
func (a *Ativa) Discar(ctx context.Context, host string, porta int32) (net.Conn, error) {
	alvo := forward.Target{Host: host, Port: porta}
	a.mu.Lock()
	m, ok := a.tuneis[alvo.String()]
	if !ok {
		m = &forward.Mapping{Target: alvo, Mode: forward.ModeMux, Dial: func(ctx context.Context, t forward.Target, mux bool) (*peer.Session, error) {
			return a.conectar(ctx, pb.ConnType_PORT_FORWARD, forward.LoginFields(t, mux), forward.LoginErrorCode)
		}}
		a.tuneis[alvo.String()] = m
	}
	a.mu.Unlock()
	nosso, deles := net.Pipe()
	go func() {
		if err := m.Serve(context.WithoutCancel(ctx), deles); err != nil {
			deles.Close()
		}
	}()
	return nosso, nil
}

// HTTP faz um pedido a partir da rede do cliente: sistema web interno, API local.
func (a *Ativa) HTTP(ctx context.Context, req *http.Request, max int64) (*http.Response, []byte, error) {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
		h, p, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		n, err := net.LookupPort("tcp", p)
		if err != nil {
			return nil, err
		}
		return a.Discar(ctx, h, int32(n))
	}, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, max))
	return resp, b, err
}

// Salvar guarda o acesso fixo no cadastro.
func (a *Ativa) Salvar(apelido, obs string) error {
	a.cred.Lock()
	m := maquinas.Maquina{Apelido: apelido, ID: a.ID, Cliente: a.Cliente, Autorizado: a.Autorizado, Em: time.Now(), Obs: obs, Hash: a.hash}
	a.cred.Unlock()
	if err := a.g.Cadastro.Salvar(m); err != nil {
		return err
	}
	a.Apelido = apelido
	return nil
}

// Fechar encerra tudo o que está aberto com a máquina.
func (a *Ativa) Fechar() {
	a.g.mu.Lock()
	if a.g.ativas[a.ID] == a {
		delete(a.g.ativas, a.ID)
	}
	a.g.mu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.term != nil {
		a.term.Fechar()
	}
	if a.admin != nil {
		a.admin.Fechar()
	}
	if a.conversa != nil {
		a.conversa.Fechar()
	}
	if a.tela != nil {
		a.tela.Fechar()
	}
	if a.arq != nil {
		a.arq.Close()
	}
	for _, m := range a.tuneis {
		m.Close()
	}
	a.cred.Lock()
	a.senha, a.senhaWin, a.hash = "", "", nil
	a.cred.Unlock()
}
